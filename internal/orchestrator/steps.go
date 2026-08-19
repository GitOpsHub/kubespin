package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/GitOpsHub/kubespin/internal/argocd"
	"github.com/GitOpsHub/kubespin/internal/catalog"
	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
	"github.com/GitOpsHub/kubespin/internal/registry"
	"github.com/GitOpsHub/kubespin/internal/repo"
)

// Cloud bundles the provisioners for one cloud. Assembling them here keeps the
// per-cloud construction in one place, so adding GCP and Azure is a matter of
// building this struct differently rather than changing the orchestrator.
type Cloud struct {
	Cluster  provisioner.ClusterProvisioner
	Identity provisioner.IdentityProvisioner
	Network  provisioner.NetworkProvisioner

	// IngestionEndpoint is the Central Ingestion API the status reporter pushes
	// to, and the only destination a cluster's egress must permit.
	IngestionEndpoint provisioner.EgressDestination

	// Wait tunes how cluster creation is polled.
	Wait provisioner.WaitOptions
}

// ProvisioningSteps builds the steps that drive a real cluster from pending to
// ready.
func ProvisioningSteps(
	cloud Cloud, repoProv repo.Provisioner, resolver catalog.Resolver, reg registry.Registry,
	installer argocd.Installer, applier argocd.KubeApplier, logger *slog.Logger,
) map[core.Phase]Step {
	if logger == nil {
		logger = slog.Default()
	}

	steps := DefaultSteps()
	steps[core.PhasePending] = StepFunc{
		Label: "create cluster",
		Fn:    createClusterStep(cloud, logger),
	}
	steps[core.PhaseClusterCreated] = StepFunc{
		Label: "bind workload identity",
		Fn:    bindIdentityStep(cloud, reg, logger),
	}
	steps[core.PhaseIdentityBound] = StepFunc{
		Label: "create and seed repository",
		Fn:    seedRepoStep(repoProv, resolver, logger),
	}
	steps[core.PhaseRepoPushed] = StepFunc{
		Label: "install argocd",
		Fn:    installArgoCDStep(cloud, installer, applier, repoProv, resolver, logger),
	}
	return steps
}

// installArgoCDStep installs Argo CD into the cluster, applies the
// self-referential root Application directly (never committed to the repo it
// manages), and commits the per-addon Applications app-of-apps discovers.
//
// It needs a *rest.Config for the cluster, which only exists once the
// cluster is active — provisioner.RESTConfigProvisioner is implemented by
// every cloud's ClusterProvisioner (internal/provisioner/{aws,gcp,azure}),
// so the type assertion here only fails for a hypothetical future provider
// that has not implemented it yet.
// restConfigFor builds the *rest.Config for spec's cluster, the prerequisite
// every direct-to-API-server call (installing/upgrading Argo CD, applying the
// root Application) needs before it can do anything.
func restConfigFor(ctx context.Context, cloud Cloud, spec core.ClusterSpec) (*rest.Config, error) {
	restConfigProv, ok := cloud.Cluster.(provisioner.RESTConfigProvisioner)
	if !ok {
		return nil, fmt.Errorf("provider %s cannot build a cluster REST config", cloud.Cluster.Provider())
	}
	restConfig, err := restConfigProv.RESTConfig(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("building REST config for %s: %w", spec.ID, err)
	}
	return restConfig, nil
}

func installArgoCDStep(
	cloud Cloud, installer argocd.Installer, applier argocd.KubeApplier,
	repoProv repo.Provisioner, resolver catalog.Resolver, logger *slog.Logger,
) func(context.Context, core.ClusterSpec, registry.Record) error {
	return func(ctx context.Context, spec core.ClusterSpec, _ registry.Record) error {
		restConfig, err := restConfigFor(ctx, cloud, spec)
		if err != nil {
			return err
		}

		profile, err := catalog.ResolveForCluster(ctx, resolver, spec)
		if err != nil {
			return fmt.Errorf("resolving profile for %s: %w", spec.ID, err)
		}

		addon, ok := profile.Addon(argocd.ReleaseName)
		if !ok {
			return fmt.Errorf("resolved profile for %s carries no argocd addon", spec.ID)
		}
		if err := installer.Install(ctx, restConfig, addon); err != nil {
			return fmt.Errorf("installing argocd for %s: %w", spec.ID, err)
		}
		logger.Info("installed argocd", "cluster", spec.ID)

		repoURL, err := repoProv.RepoURL(ctx, spec)
		if err != nil {
			return fmt.Errorf("resolving repository URL for %s: %w", spec.ID, err)
		}

		// The repository is always private, so without this Secret the root
		// Application below fails its first reconcile with "authentication
		// required" and never discovers a single addon. Applied before the
		// root Application so the credential already exists the moment Argo
		// CD's repo-server first tries to use it.
		username, password := repoProv.Credentials()
		repoCreds, err := argocd.RenderRepoCredentialsSecret(repoURL, username, password)
		if err != nil {
			return fmt.Errorf("rendering repository credentials for %s: %w", spec.ID, err)
		}
		if err := applier.Apply(ctx, restConfig, repoCreds); err != nil {
			return fmt.Errorf("applying repository credentials for %s: %w", spec.ID, err)
		}
		logger.Info("applied repository credentials", "cluster", spec.ID)

		rootApp, err := argocd.RenderRootApplication(repoURL)
		if err != nil {
			return fmt.Errorf("rendering root Application for %s: %w", spec.ID, err)
		}
		if err := applier.Apply(ctx, restConfig, rootApp); err != nil {
			return fmt.Errorf("applying root Application for %s: %w", spec.ID, err)
		}
		logger.Info("applied app-of-apps root application", "cluster", spec.ID)

		committed, err := repo.ReconcileAppOfApps(ctx, repoProv, spec, profile)
		if err != nil {
			return fmt.Errorf("committing app-of-apps for %s: %w", spec.ID, err)
		}
		if committed {
			logger.Info("committed app-of-apps addon applications", "cluster", spec.ID)
		}
		return nil
	}
}

// seedRepoStep creates the cluster's repository (idempotent) and commits its
// initial cluster.yaml, addons.yaml, and .state.yaml.
func seedRepoStep(
	repoProv repo.Provisioner, resolver catalog.Resolver, logger *slog.Logger,
) func(context.Context, core.ClusterSpec, registry.Record) error {
	return func(ctx context.Context, spec core.ClusterSpec, _ registry.Record) error {
		profile, err := catalog.ResolveForCluster(ctx, resolver, spec)
		if err != nil {
			return fmt.Errorf("resolving profile for %s: %w", spec.ID, err)
		}

		if err := repo.Seed(ctx, repoProv, spec, profile); err != nil {
			return fmt.Errorf("seeding repository for %s: %w", spec.ID, err)
		}

		logger.Info("seeded cluster repository", "cluster", spec.ID, "profile", spec.Profile)
		return nil
	}
}

// ReadyReconcile builds the ReconcileFunc that keeps a ready cluster
// converged on every subsequent `apply`: an infra diff routes to
// cloud.Cluster.Reconcile, an addon diff routes to repo.ReconcileAddons, and a
// no-change run makes neither call. See WithReadyReconcile.
//
// Argo CD's own release is the one addon app-of-apps cannot converge, since it
// cannot sync itself — installArgoCDStep is the only other caller of
// installer.Install, and that only runs once, on the PhaseRepoPushed->
// PhaseArgoCDInstalled transition. Without re-running it here, a
// cluster.yaml override touching the "argocd" addon (e.g. exposing
// argocd-server) would commit into addons.yaml on every subsequent apply but
// never actually reach the live release. installer.Install is documented
// safe to call on every apply (see the Installer interface), so this costs
// nothing on the common no-change path.
func ReadyReconcile(
	cloud Cloud, installer argocd.Installer, repoProv repo.Provisioner, resolver catalog.Resolver, logger *slog.Logger,
) ReconcileFunc {
	if logger == nil {
		logger = slog.Default()
	}

	return func(ctx context.Context, spec core.ClusterSpec, _ registry.Record) error {
		change, err := cloud.Cluster.Reconcile(ctx, spec)
		if err != nil {
			return fmt.Errorf("reconciling infra for %s: %w", spec.ID, err)
		}
		if change.Changed {
			logger.Info("reconciled cluster infra", "cluster", spec.ID, "changes", change.Details)
		}

		profile, err := catalog.ResolveForCluster(ctx, resolver, spec)
		if err != nil {
			return fmt.Errorf("resolving profile for %s: %w", spec.ID, err)
		}

		addon, ok := profile.Addon(argocd.ReleaseName)
		if !ok {
			return fmt.Errorf("resolved profile for %s carries no argocd addon", spec.ID)
		}
		restConfig, err := restConfigFor(ctx, cloud, spec)
		if err != nil {
			return err
		}
		if err := installer.Install(ctx, restConfig, addon); err != nil {
			return fmt.Errorf("reconciling argocd release for %s: %w", spec.ID, err)
		}

		committed, err := repo.ReconcileAddons(ctx, repoProv, spec, profile)
		if err != nil {
			return fmt.Errorf("reconciling addons for %s: %w", spec.ID, err)
		}
		if committed {
			logger.Info("committed addon changes", "cluster", spec.ID, "profile", spec.Profile)
		}

		// addons.yaml above is the informational record of the resolved
		// profile; apps/*.yaml is what Argo CD's app-of-apps root Application
		// actually watches and syncs from. Without also reconciling this, an
		// override on any Argo-CD-synced addon (cilium, cert-manager, ...)
		// would land in addons.yaml but never reach the Application Argo CD
		// reads, so it would never sync — installArgoCDStep is the only other
		// caller of ReconcileAppOfApps, and that runs once, at initial
		// install.
		appsCommitted, err := repo.ReconcileAppOfApps(ctx, repoProv, spec, profile)
		if err != nil {
			return fmt.Errorf("reconciling app-of-apps for %s: %w", spec.ID, err)
		}
		if appsCommitted {
			logger.Info("committed app-of-apps changes", "cluster", spec.ID, "profile", spec.Profile)
		}
		return nil
	}
}

// Teardown builds the TeardownFunc that performs M9's reverse teardown:
// identity cleanup, cluster delete, repo archive — deliberately in that
// order, opposite of Apply's create-cluster-then-bind-identity, so nothing
// is deleted while another step might still need it.
//
// Every step here is idempotent (Deprovision/Delete/Archive on something
// already gone converge rather than error), which is what lets Delete retry
// this whole function after a partial failure instead of needing to track
// which sub-step it reached.
func Teardown(cloud Cloud, repoProv repo.Provisioner, logger *slog.Logger) TeardownFunc {
	if logger == nil {
		logger = slog.Default()
	}

	return func(ctx context.Context, spec core.ClusterSpec, _ registry.Record) error {
		comp := provisioner.StatusReporter()
		if err := cloud.Identity.Deprovision(ctx, spec, comp); err != nil {
			return fmt.Errorf("deprovisioning identity for %s: %w", spec.ID, err)
		}
		logger.Info("deprovisioned workload identity", "cluster", spec.ID, "component", comp.Name)

		// Must happen before the cluster itself goes: a Kubernetes Service of
		// type LoadBalancer (Argo CD's own exposure, or an addon's) owns a real
		// cloud load balancer that deleting the cluster does not clean up on its
		// own, left orphaned — billing indefinitely — and blocking the VPC/
		// network teardown below with a dependency violation.
		if err := drainLoadBalancers(ctx, cloud, spec, logger); err != nil {
			return fmt.Errorf("draining load balancers for %s: %w", spec.ID, err)
		}

		// Node pools drain before the control plane goes, so this call blocks
		// for minutes; say so rather than looking hung.
		logger.Info("deleting cluster; node pools drain first, this takes several minutes",
			"cluster", spec.ID)
		if err := cloud.Cluster.Delete(ctx, spec); err != nil {
			return fmt.Errorf("deleting cluster %s: %w", spec.ID, err)
		}

		// Delete only requests the teardown. Waiting for the cloud to report the
		// cluster gone is what makes the decommissioned phase honest: the caller
		// records it only after this returns.
		if err := provisioner.WaitUntilGone(ctx, cloud.Cluster, spec, cloud.Wait); err != nil {
			return fmt.Errorf("waiting for cluster %s to be deleted: %w", spec.ID, err)
		}
		logger.Info("deleted cluster", "cluster", spec.ID)

		// Reverses EnsureNetwork, symmetric with createClusterStep calling it on
		// the way up. Identified by deterministic name rather than spec.Subnets,
		// so this is safe even when delete was not given the same --subnets an
		// earlier apply was.
		if cloud.Network != nil {
			if err := cloud.Network.DeleteNetwork(ctx, spec); err != nil {
				return fmt.Errorf("deleting network for %s: %w", spec.ID, err)
			}
			logger.Info("deleted network", "cluster", spec.ID)
		}

		if err := repoProv.Archive(ctx, spec); err != nil {
			return fmt.Errorf("archiving repository for %s: %w", spec.ID, err)
		}
		logger.Info("archived cluster repository", "cluster", spec.ID)

		return nil
	}
}

// drainLoadBalancersTimeout/drainLoadBalancersPollInterval bound how long
// Teardown waits for a deleted Service's cloud load balancer to actually tear
// down before giving up and proceeding with cluster deletion anyway — a stuck
// drain must not wedge delete forever.
const (
	drainLoadBalancersTimeout      = 3 * time.Minute
	drainLoadBalancersPollInterval = 5 * time.Second
)

// drainLoadBalancers deletes every Service of type LoadBalancer across all
// namespaces and waits for each to actually disappear from the API before
// returning.
//
// If the cluster cannot be reached at all — already deleted by an earlier,
// interrupted teardown, or never became active in the first place — this is
// a no-op rather than a failure: there is nothing to drain from a cluster
// that is not there, and a resumed delete must still be able to converge.
func drainLoadBalancers(ctx context.Context, cloud Cloud, spec core.ClusterSpec, logger *slog.Logger) error {
	restConfig, err := restConfigFor(ctx, cloud, spec)
	if err != nil {
		logger.Debug("skipping load balancer drain; cluster is not reachable",
			"cluster", spec.ID, "error", err)
		return nil
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("building Kubernetes client for %s: %w", spec.ID, err)
	}

	services, err := clientset.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		// Same reasoning as the RESTConfig failure above: an unreachable API
		// server here means there is nothing left to drain, not a teardown
		// failure.
		logger.Debug("skipping load balancer drain; could not list services",
			"cluster", spec.ID, "error", err)
		return nil
	}

	var toDelete []corev1.Service
	for _, svc := range services.Items {
		if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
			toDelete = append(toDelete, svc)
		}
	}
	if len(toDelete) == 0 {
		return nil
	}

	logger.Info("draining LoadBalancer services before cluster deletion",
		"cluster", spec.ID, "count", len(toDelete))
	for _, svc := range toDelete {
		err := clientset.CoreV1().Services(svc.Namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting service %s/%s: %w", svc.Namespace, svc.Name, err)
		}
	}

	deadline := time.Now().Add(drainLoadBalancersTimeout)
	for {
		remaining := 0
		for _, svc := range toDelete {
			_, err := clientset.CoreV1().Services(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{})
			switch {
			case err == nil:
				remaining++
			case apierrors.IsNotFound(err):
			default:
				return fmt.Errorf("checking service %s/%s: %w", svc.Namespace, svc.Name, err)
			}
		}
		if remaining == 0 {
			logger.Info("load balancer services drained", "cluster", spec.ID)
			return nil
		}
		if time.Now().After(deadline) {
			logger.Warn(
				"load balancer services did not finish draining in time; proceeding with cluster deletion anyway",
				"cluster", spec.ID, "still_present", remaining)
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(drainLoadBalancersPollInterval):
		}
	}
}

// createClusterStep resolves the cluster's network, requests the cluster,
// waits for it to become active, then reconciles node pools and opens the
// status reporter's egress path.
//
// Waiting here rather than returning early is deliberate: the phase is only
// recorded once the cluster is genuinely usable, so a resumed run never
// believes a still-creating cluster is finished.
func createClusterStep(
	cloud Cloud, logger *slog.Logger,
) func(context.Context, core.ClusterSpec, registry.Record) error {
	return func(ctx context.Context, spec core.ClusterSpec, _ registry.Record) error {
		if cloud.Network != nil {
			result, err := cloud.Network.EnsureNetwork(ctx, spec)
			if err != nil {
				return fmt.Errorf("ensuring network for %s: %w", spec.ID, err)
			}
			// spec is this closure's own local copy, safe to mutate: everything
			// below, including Cluster.Create, must see the resolved subnets.
			spec.Subnets = result.SubnetIDs
			if result.Change.Changed {
				logger.Info("provisioned network", "cluster", spec.ID, "changes", result.Change.Details)
			}
		}

		if err := cloud.Cluster.Create(ctx, spec); err != nil {
			return fmt.Errorf("requesting cluster %s: %w", spec.ID, err)
		}

		logger.Info("waiting for the control plane", "cluster", spec.ID)
		if _, err := provisioner.WaitUntilActive(ctx, cloud.Cluster, spec, cloud.Wait); err != nil {
			return fmt.Errorf("waiting for cluster %s: %w", spec.ID, err)
		}

		// Node pools are attached after the control plane is active, so this
		// reconcile is what actually creates them on a first run.
		change, err := cloud.Cluster.Reconcile(ctx, spec)
		if err != nil {
			return fmt.Errorf("reconciling cluster %s: %w", spec.ID, err)
		}
		if change.Changed {
			logger.Info("reconciled cluster", "cluster", spec.ID, "changes", change.Details)
		}

		return openEgress(ctx, cloud, spec, logger)
	}
}

// openEgress opens the one outbound path fleet state travels on.
func openEgress(
	ctx context.Context, cloud Cloud, spec core.ClusterSpec, logger *slog.Logger,
) error {
	if cloud.Network == nil || cloud.IngestionEndpoint.Host == "" {
		// Without a configured ingestion endpoint there is nothing to allow.
		// Said out loud, because a cluster that cannot report is invisible to
		// the fleet rather than obviously broken.
		logger.Warn("no ingestion endpoint configured; the cluster will not be able to report status",
			"cluster", spec.ID)
		return nil
	}

	change, err := cloud.Network.AllowEgress(ctx, spec, cloud.IngestionEndpoint)
	if err != nil {
		return fmt.Errorf("opening egress to the ingestion API: %w", err)
	}
	if change.Changed {
		logger.Info("opened egress", "cluster", spec.ID, "changes", change.Details)
	}
	return nil
}

// bindIdentityStep gives the status reporter a cloud-native identity, and
// records the cluster's OIDC issuer in the Fleet Registry.
//
// That issuer is what the Central Ingestion API (M6) verifies
// fleet-status-reporter's signature against, so it has to be captured
// somewhere durable, and once, right when it becomes available — Describe
// only reports it once the control plane is active, which by this phase it
// is.
func bindIdentityStep(
	cloud Cloud, reg registry.Registry, logger *slog.Logger,
) func(context.Context, core.ClusterSpec, registry.Record) error {
	return func(ctx context.Context, spec core.ClusterSpec, _ registry.Record) error {
		comp := provisioner.StatusReporter()

		binding, err := cloud.Identity.ProvisionForComponent(ctx, spec, comp)
		if err != nil {
			return fmt.Errorf("binding identity for %s: %w", comp.Name, err)
		}

		logger.Info("bound workload identity",
			"cluster", spec.ID, "component", comp.Name, "identity", binding.Identifier)

		state, err := cloud.Cluster.Describe(ctx, spec)
		if err != nil {
			return fmt.Errorf("describing %s to record its OIDC issuer: %w", spec.ID, err)
		}
		if state.OIDCIssuer == "" {
			return nil
		}
		if err := reg.RecordOIDCIssuer(ctx, spec.ID, state.OIDCIssuer); err != nil {
			return fmt.Errorf("recording OIDC issuer for %s: %w", spec.ID, err)
		}
		return nil
	}
}
