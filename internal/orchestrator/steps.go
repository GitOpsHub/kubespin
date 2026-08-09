package orchestrator

import (
	"context"
	"fmt"
	"log/slog"

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
//
// The Argo CD bootstrap (M5) remains a no-op, so a run reaches ready with a
// real cluster, a real workload identity, and a seeded repository, but no
// addons synced — which is exactly the M3 gate and no more.
func ProvisioningSteps(
	cloud Cloud, repoProv repo.Provisioner, resolver catalog.Resolver, reg registry.Registry, logger *slog.Logger,
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
	return steps
}

// resolveProfile resolves spec's profile, applies its per-cluster override
// patch, and templates ingress/Gateway addons for spec's access mode, so
// every caller in this file renders the same resolved addon set rather than
// each reimplementing the resolve-merge-template sequence.
func resolveProfile(ctx context.Context, resolver catalog.Resolver, spec core.ClusterSpec) (core.Profile, error) {
	profile, err := resolver.Resolve(ctx, spec.Profile)
	if err != nil {
		return core.Profile{}, fmt.Errorf("resolving profile %s for %s: %w", spec.Profile, spec.ID, err)
	}

	merged, err := catalog.Merge(profile, spec.Overrides)
	if err != nil {
		return core.Profile{}, fmt.Errorf("applying overrides for %s: %w", spec.ID, err)
	}

	return argocd.ApplyProfileIngressDefaults(spec.Access, merged), nil
}

// seedRepoStep creates the cluster's repository (idempotent) and commits its
// initial cluster.yaml, addons.yaml, and .state.yaml.
func seedRepoStep(
	repoProv repo.Provisioner, resolver catalog.Resolver, logger *slog.Logger,
) func(context.Context, core.ClusterSpec, registry.Record) error {
	return func(ctx context.Context, spec core.ClusterSpec, _ registry.Record) error {
		profile, err := resolveProfile(ctx, resolver, spec)
		if err != nil {
			return err
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
func ReadyReconcile(cloud Cloud, repoProv repo.Provisioner, resolver catalog.Resolver, logger *slog.Logger) ReconcileFunc {
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

		profile, err := resolveProfile(ctx, resolver, spec)
		if err != nil {
			return err
		}

		committed, err := repo.ReconcileAddons(ctx, repoProv, spec, profile)
		if err != nil {
			return fmt.Errorf("reconciling addons for %s: %w", spec.ID, err)
		}
		if committed {
			logger.Info("committed addon changes", "cluster", spec.ID, "profile", spec.Profile)
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

		if err := cloud.Cluster.Delete(ctx, spec); err != nil {
			return fmt.Errorf("deleting cluster %s: %w", spec.ID, err)
		}
		logger.Info("deleted cluster", "cluster", spec.ID)

		if err := repoProv.Archive(ctx, spec); err != nil {
			return fmt.Errorf("archiving repository for %s: %w", spec.ID, err)
		}
		logger.Info("archived cluster repository", "cluster", spec.ID)

		return nil
	}
}

// createClusterStep requests the cluster, waits for it to become active, then
// reconciles node pools and opens the status reporter's egress path.
//
// Waiting here rather than returning early is deliberate: the phase is only
// recorded once the cluster is genuinely usable, so a resumed run never
// believes a still-creating cluster is finished.
func createClusterStep(
	cloud Cloud, logger *slog.Logger,
) func(context.Context, core.ClusterSpec, registry.Record) error {
	return func(ctx context.Context, spec core.ClusterSpec, _ registry.Record) error {
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
