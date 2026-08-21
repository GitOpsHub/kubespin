package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/GitOpsHub/kubespin/internal/argocd"
	"github.com/GitOpsHub/kubespin/internal/catalog"
	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/kubeconfig"
	"github.com/GitOpsHub/kubespin/internal/orchestrator"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
	awsprov "github.com/GitOpsHub/kubespin/internal/provisioner/aws"
	azureprov "github.com/GitOpsHub/kubespin/internal/provisioner/azure"
	gcpprov "github.com/GitOpsHub/kubespin/internal/provisioner/gcp"
	"github.com/GitOpsHub/kubespin/internal/registry"
	"github.com/GitOpsHub/kubespin/internal/repo"
)

func newApplyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Create or reconcile a cluster to match its desired state",
		Long: `apply drives the full provisioning state machine: acquire the cluster lease,
create the cluster, bind workload identity, create and seed its repository,
install Argo CD, and mark the cluster ready.

apply is idempotent and resumable. A repeat run with no changes performs no
cloud calls and produces no commits; a failed run resumes from the phase
recorded in the Fleet Registry.

The spec may come from a cluster.yaml — the same file the cluster's repository
holds — or from the flags below, which override the file when given.

Installing Argo CD is not a push from inside the cluster: apply connects
directly to the API server (via the Helm SDK) from whatever machine runs this
command. For --access private, that means the operator's machine needs
network reachability into the cluster's VPC/VNet (VPN, peering, or a bastion)
— without it, apply will get stuck at the "install argocd" step with a DNS or
connection-timeout error. --access public avoids that, but on GCP it is not
enough by itself: GKE enables master-authorized-networks with an empty
allowlist by default, so nothing (not even the operator) can reach the public
endpoint until --authorized-cidrs includes the caller's IP. AWS and Azure
public endpoints are open to 0.0.0.0/0 unless --authorized-cidrs is set.

That step waits for Argo CD to actually be running, not just for its
manifests to be accepted, so it takes a few minutes on a fresh cluster while
images are pulled. An Argo CD that never becomes ready — pods that cannot be
scheduled or cannot pull — fails the step there rather than looking like
addons that silently never sync.`,
		Example: `  # AWS, private API server, default node pool, default size (small)
  kubespin apply --provider aws --region us-east-1 --cluster-id demo-aws \
    --access private \
    --github-org GitOpsHub

  # GCP, public API server, larger node pool — authorized-cidrs is required on GCP
  # for the operator's own machine to reach the endpoint and install Argo CD
  kubespin apply --provider gcp --gcp-project kubernetes-dev-502710 --region us-central1 \
    --cluster-id demo-gcp --access public --authorized-cidrs 203.0.113.4/32 \
    --size small \
    --instance-type e2-standard-4 --desired-size 3 \
    --github-org GitOpsHub

  # Azure, medium size (adds Velero + Falco onto the default addon set)
  kubespin apply --provider azure --azure-subscription 3df9adbd-ea55-4c92-964c-0252031979de --region eastus \
    --cluster-id demo-azure --access private --size medium \
    --instance-type Standard_D4s_v7 \
    --github-org GitOpsHub

  # Preview what apply would do without touching any cloud
  kubespin apply --spec ./cluster.yaml --dry-run`,
		Args: cobra.NoArgs,
		RunE: runApply,
	}

	fs := cmd.Flags()
	fs.String("spec", "", "path to a cluster.yaml describing the cluster")
	fs.String("cluster-id", "", "cluster identifier (also the repository suffix)")
	fs.String("provider", "", "cloud provider: aws, gcp, or azure")
	fs.String("region", "", "cloud region")
	fs.String("access", string(core.AccessPrivate), "API server exposure: private or public")
	fs.String("size", "small", "cluster size: small, medium, or large — determines the default addon set. Argo CD and an autoscaler (Karpenter on AWS, cluster-autoscaler on GCP/Azure) ship at every size; medium adds Velero+Falco, large adds strict Kyverno policies + audit logging + OTel")
	fs.String("kubernetes-version", "", "Kubernetes minor version, e.g. 1.34")
	fs.StringSlice("subnets", nil, "existing subnets to place the cluster in")
	fs.StringSlice("authorized-cidrs", nil, "CIDR blocks allowed to reach the API server when --access public (GCP: required to reach the endpoint at all, since GKE enables master-authorized-networks with an empty allowlist by default; AWS/Azure: public endpoints are open to 0.0.0.0/0 unless this is set)")
	fs.String("vpc-cidr", "", "address space for the VPC kubespin creates when --subnets is omitted (AWS only, default 10.0.0.0/16)")
	fs.String("vnet-cidr", "", "address space for the VNet kubespin creates when --subnets is omitted (Azure only, default 10.0.0.0/16)")
	fs.String("subnet-cidr", "", "address prefix for the subnet kubespin creates when --subnets is omitted (Azure default 10.0.1.0/24, GCP default 10.0.0.0/20)")
	fs.String("ingestion-endpoint", "", "Central Ingestion API host the cluster must be able to reach")
	fs.String("gcp-project", "", "GCP project hosting the cluster (required for --provider gcp)")
	fs.String("azure-subscription", "", "Azure subscription hosting the cluster (required for --provider azure)")
	fs.String("github-org", "", "GitHub organization cluster repositories are created in")
	fs.String("github-base-url", "", "GitHub Enterprise API base URL (leave empty for github.com)")
	fs.String("github-upload-url", "", "GitHub Enterprise upload URL (leave empty for github.com)")

	fs.String("instance-type", "m6i.large", "instance type for the default node pool (defaults to a cloud-appropriate value per --provider when unset: m6i.large on aws, e2-standard-4 on gcp, Standard_D4s_v7 on azure; --spot picks a smaller cloud-appropriate default instead, see --spot)")
	fs.Int32("min-size", 1, "minimum size of the default node pool (--spot defaults this lower, see --spot)")
	fs.Int32("max-size", 5, "maximum size of the default node pool (--spot defaults this lower, see --spot)")
	fs.Int32("desired-size", 2, "desired size of the default node pool (--spot defaults this lower, see --spot)")
	fs.Int32("disk-size", 0, "boot disk size in GB for the default node pool's nodes (0 = cloud default; GKE regional clusters multiply this by the number of zones, so it is worth setting explicitly on quota-constrained projects; --spot picks a smaller default, see --spot)")
	fs.Bool("spot", false, "one flag for the cheapest dev/learning cluster on any cloud: spot/preemptible instances (AWS/GCP; AKS's default pool must stay on-demand, so this part is a no-op on --provider azure), plus a smaller default --instance-type/--min-size/--max-size/--desired-size/--disk-size sized to still run the default (--size small) addon set (t3.medium/e2-medium/Standard_B2s, 1/2/1 nodes) — pass any of those flags explicitly to override just that piece. On GCP this also switches to a zonal cluster (eligible for GCP's free zonal-cluster tier) and gives nodes public IPs instead of provisioning Cloud NAT, unless --zone/--gcp-public-nodes override it.")
	fs.String("zone", "", "GCP zone (e.g. us-central1-a) requesting a zonal GKE cluster instead of the default regional one (GCP only). --spot already sets this; only needed to pick a specific zone, or to go zonal without spot.")
	fs.Bool("gcp-public-nodes", false, "give GKE nodes public IPs instead of provisioning a Cloud Router + Cloud NAT for them (GCP only). --spot already enables this; only needed to use it without spot.")

	fs.Bool("update-kubeconfig", true, "update the local kubeconfig with a context for this cluster once apply succeeds, by shelling out to aws/gcloud/az (disable with --update-kubeconfig=false)")
	fs.String("kubeconfig", "", "path to the kubeconfig file to update when --update-kubeconfig is set (defaults to the cloud CLI's own default, typically ~/.kube/config or $KUBECONFIG)")

	return cmd
}

func runApply(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	logger := LoggerFrom(ctx)

	spec, err := loadSpec(cmd)
	if err != nil {
		return err
	}

	cfg, reg, err := registryPrereqs(cmd)
	if err != nil {
		return err
	}

	// A dry run only reads the AWS-hosted Fleet Registry — it never touches the
	// cluster's own cloud — so it only needs AWS authenticated, regardless of
	// spec.Provider. A real apply needs both.
	authProviders := []string{"aws"}
	if !cfg.DryRun {
		authProviders = cloudAuthProviders(spec)
	}
	if err := ensureAuthenticated(cmd, authProviders...); err != nil {
		return err
	}

	if cfg.DryRun {
		return reportPlan(ctx, cmd, reg, spec)
	}

	cloud, err := buildCloud(ctx, cmd, spec)
	if err != nil {
		return err
	}

	repoClients, err := buildRepoClients(cmd)
	if err != nil {
		return err
	}
	repoProv := repo.NewProvisioner(repoClients, repo.WithLogger(logger))

	resolver := catalog.NewBuiltinResolver()

	installer := argocd.NewHelmInstaller(logger)
	o := orchestrator.New(reg,
		orchestrator.WithSteps(orchestrator.ProvisioningSteps(
			cloud, repoProv, resolver, reg,
			installer, argocd.NewDynamicApplier(logger), logger,
		)),
		orchestrator.WithReadyReconcile(orchestrator.ReadyReconcile(cloud, installer, repoProv, resolver, logger)),
		orchestrator.WithLogger(logger),
	)

	rec, err := o.Apply(ctx, spec)
	if err != nil {
		// The phase reached is worth printing even on failure: it is where the
		// next run resumes from.
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "cluster %s stopped at phase %s\n", spec.ID, rec.Phase)
		return fmt.Errorf("applying %s: %w", spec.ID, err)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "cluster %s is %s\n", rec.ClusterID, rec.Phase)

	var kubeContext string
	if update, err := cmd.Flags().GetBool("update-kubeconfig"); err != nil {
		return fmt.Errorf("reading --update-kubeconfig: %w", err)
	} else if update {
		kubeContext = updateLocalKubeconfig(ctx, cmd, logger, spec)
	}

	if rec.Phase == core.PhaseReady {
		captureAndRecordArgoCDAccess(ctx, logger, reg, cloud, spec, kubeContext)
	}

	printAccessSummary(cmd, spec, kubeContext)

	return nil
}

// updateLocalKubeconfig adds or refreshes a kubeconfig context for spec's
// cluster once apply has reached phase ready, returning the context name it
// wrote (empty on failure). It is best-effort: the cluster is already fully
// provisioned at this point, so a local kubeconfig hiccup (CLI missing,
// stale cached auth) is logged as a warning rather than failing the overall
// apply command — the same reasoning as the non-fatal branch protection
// warning in internal/repo.
func updateLocalKubeconfig(ctx context.Context, cmd *cobra.Command, logger *slog.Logger, spec core.ClusterSpec) string {
	path, _ := cmd.Flags().GetString("kubeconfig")
	gcpProject, _ := cmd.Flags().GetString("gcp-project")
	azureSub, _ := cmd.Flags().GetString("azure-subscription")

	contextName, err := kubeconfig.Update(ctx, spec, kubeconfig.Options{
		Path:              path,
		GCPProject:        gcpProject,
		AzureSubscription: azureSub,
	})
	if err != nil {
		logger.Warn("could not update local kubeconfig", "cluster", spec.ID, "error", err)
		return ""
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "updated kubeconfig context for %s\n", spec.ID)
	return contextName
}

const (
	argoCDAccessWaitTimeout  = 2 * time.Minute
	argoCDAccessPollInterval = 5 * time.Second

	argoCDAdminSecretName = "argocd-initial-admin-secret" // #nosec G101 -- Secret name, not a credential
	argoCDAdminUsername   = "admin"
)

// captureAndRecordArgoCDAccess captures the cluster's Argo CD LoadBalancer
// endpoint and admin credentials and persists them to the Fleet Registry's
// cluster_argocd_details table. It runs on every apply that reaches
// PhaseReady — including a no-op reconcile against an already-ready cluster
// — so the row stays current and a failed capture gets another chance next
// run.
//
// Best-effort: the cluster is already fully provisioned by the time this
// runs, so any failure here (REST config, LoadBalancer never assigned, the
// admin secret missing) is logged as a warning and does not fail the apply.
func captureAndRecordArgoCDAccess(
	ctx context.Context, logger *slog.Logger, reg registry.Registry,
	cloud orchestrator.Cloud, spec core.ClusterSpec, kubeContext string,
) {
	restConfigProv, ok := cloud.Cluster.(provisioner.RESTConfigProvisioner)
	if !ok {
		logger.Warn("skipping argocd access capture; provider cannot build a cluster REST config",
			"cluster", spec.ID, "provider", cloud.Cluster.Provider())
		return
	}
	restConfig, err := restConfigProv.RESTConfig(ctx, spec)
	if err != nil {
		logger.Warn("skipping argocd access capture; could not build cluster REST config",
			"cluster", spec.ID, "error", err)
		return
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		logger.Warn("skipping argocd access capture; could not build Kubernetes client",
			"cluster", spec.ID, "error", err)
		return
	}

	endpoint, err := waitForArgoCDEndpoint(ctx, clientset)
	if err != nil {
		logger.Warn("skipping argocd access capture; argocd-server LoadBalancer endpoint not ready",
			"cluster", spec.ID, "error", err)
		return
	}

	secret, err := clientset.CoreV1().Secrets(argocd.Namespace).
		Get(ctx, argoCDAdminSecretName, metav1.GetOptions{})
	if err != nil {
		logger.Warn("skipping argocd access capture; could not read admin secret",
			"cluster", spec.ID, "error", err)
		return
	}
	password := string(secret.Data["password"])
	if password == "" {
		logger.Warn("skipping argocd access capture; admin secret has no password", "cluster", spec.ID)
		return
	}

	access := registry.ArgoCDAccess{
		Provider:    spec.Provider,
		Region:      spec.Region,
		KubeContext: kubeContext,
		Endpoint:    endpoint,
		Username:    argoCDAdminUsername,
		Password:    password,
	}
	if err := reg.RecordArgoCDAccess(ctx, spec.ID, access); err != nil {
		logger.Warn("could not record argocd access details", "cluster", spec.ID, "error", err)
		return
	}
	logger.Info("recorded argocd access details", "cluster", spec.ID, "endpoint", endpoint)
}

// waitForArgoCDEndpoint polls the argocd-server Service for a LoadBalancer
// ingress IP or hostname, bounded to argoCDAccessWaitTimeout — a fresh
// LoadBalancer can take a few minutes to provision, so this does not treat
// the first empty read as failure.
func waitForArgoCDEndpoint(ctx context.Context, clientset kubernetes.Interface) (string, error) {
	deadline := time.Now().Add(argoCDAccessWaitTimeout)
	for {
		svc, err := clientset.CoreV1().Services(argocd.Namespace).
			Get(ctx, "argocd-server", metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("getting argocd-server service: %w", err)
		}
		if endpoint := loadBalancerEndpoint(svc); endpoint != "" {
			return endpoint, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("argocd-server LoadBalancer endpoint not assigned within %s", argoCDAccessWaitTimeout)
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(argoCDAccessPollInterval):
		}
	}
}

// loadBalancerEndpoint returns svc's first LoadBalancer ingress IP or
// hostname, or "" if none is assigned yet.
func loadBalancerEndpoint(svc *corev1.Service) string {
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			return ingress.IP
		}
		if ingress.Hostname != "" {
			return ingress.Hostname
		}
	}
	return ""
}

// printAccessSummary prints how to reach the cluster and its local Argo CD
// once apply succeeds: the kubectl context, and the LoadBalancer + admin
// credential commands for Argo CD's UI (see internal/argocd.DefaultAddon /
// ServerLoadBalancerValues) — a cloud LoadBalancer address takes a few
// minutes to provision, so the external-IP lookup is printed alongside it
// rather than resolved here.
func printAccessSummary(cmd *cobra.Command, spec core.ClusterSpec, kubeContext string) {
	out := cmd.OutOrStdout()

	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Connect to the cluster:")
	if kubeContext != "" {
		_, _ = fmt.Fprintf(out, "  kubectl config use-context %s\n", kubeContext)
	}
	_, _ = fmt.Fprintln(out, "  kubectl get nodes")

	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Argo CD (LoadBalancer; external IP may take a few minutes to provision):")
	_, _ = fmt.Fprintf(out, "  kubectl -n %s get svc argocd-server -w\n", argocd.Namespace)
	_, _ = fmt.Fprintln(out, "  open https://<EXTERNAL-IP>")
	_, _ = fmt.Fprintln(out, "  username: admin")
	_, _ = fmt.Fprintf(out,
		"  password: kubectl -n %s get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d; echo\n",
		argocd.Namespace)

	if org, _ := cmd.Flags().GetString("github-org"); org != "" {
		repoName := "kubespin-" + spec.ID.String()
		_, _ = fmt.Fprintln(out)
		if baseURL, _ := cmd.Flags().GetString("github-base-url"); baseURL == "" {
			_, _ = fmt.Fprintf(out, "Cluster repo: https://github.com/%s/%s\n", org, repoName)
		} else {
			// A GitHub Enterprise web UI's host is not reliably derivable from
			// its API base URL, so this stays a slug rather than a guessed link.
			_, _ = fmt.Fprintf(out, "Cluster repo: %s/%s\n", org, repoName)
		}
	}
}

// reportPlan describes what a run would do without touching any cloud.
//
// It reads the registry — which is not a mutation — and reports the phase a run
// would resume from, so an operator can see whether apply would create a
// cluster or pick up a half-finished one.
func reportPlan(
	ctx context.Context, cmd *cobra.Command, reg registry.Registry, spec core.ClusterSpec,
) error {
	out := cmd.OutOrStdout()

	rec, err := reg.Get(ctx, spec.ID)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		_, _ = fmt.Fprintf(out, "cluster %s is not registered; apply would create it from phase %s\n",
			spec.ID, core.PhasePending)
		return nil
	case err != nil:
		return fmt.Errorf("reading %s: %w", spec.ID, err)
	}

	if rec.Phase == core.PhaseReady {
		_, _ = fmt.Fprintf(out, "cluster %s is already ready; apply would run no steps\n", spec.ID)
		return nil
	}

	_, _ = fmt.Fprintf(out, "cluster %s is at phase %s; apply would resume there\n", spec.ID, rec.Phase)
	for phase := rec.Phase; phase != core.PhaseReady; {
		next, ok := phase.Next()
		if !ok {
			break
		}
		_, _ = fmt.Fprintf(out, "  %s -> %s\n", phase, next)
		phase = next
	}
	return nil
}

// cloudAuthProviders names the auth providers a real (non-dry-run) apply or
// delete needs: the cluster's own cloud, plus AWS, which is always required
// because the Fleet Registry is AWS-hosted regardless of spec.Provider.
func cloudAuthProviders(spec core.ClusterSpec) []string {
	if spec.Provider == core.ProviderAWS {
		return []string{"aws"}
	}
	return []string{"aws", string(spec.Provider)}
}

// buildCloud assembles the provisioners for the spec's cloud.
func buildCloud(ctx context.Context, cmd *cobra.Command, spec core.ClusterSpec) (orchestrator.Cloud, error) {
	// Only apply declares --ingestion-endpoint: it is the sole caller that
	// opens egress. delete (teardown) and fleet audit (read-only Describe)
	// share buildCloud but have no use for the destination, so the flag is
	// looked up rather than demanded — requiring it would make those two
	// commands fail before doing any work.
	endpoint := ""
	if f := cmd.Flags().Lookup("ingestion-endpoint"); f != nil {
		endpoint = f.Value.String()
	}

	logger := LoggerFrom(ctx)

	switch spec.Provider {
	case core.ProviderAWS:
		clients, err := awsprov.NewClients(ctx, spec.Region, awsprov.WithLogger(logger))
		if err != nil {
			return orchestrator.Cloud{}, fmt.Errorf("building AWS clients: %w", err)
		}

		return orchestrator.Cloud{
			Cluster:  awsprov.NewClusterProvisioner(clients),
			Identity: awsprov.NewIdentityProvisioner(clients),
			Network:  awsprov.NewNetworkProvisioner(clients),
			IngestionEndpoint: provisioner.EgressDestination{
				Host:        endpoint,
				Port:        443,
				Description: "kubespin fleet-status-reporter egress",
			},
			Wait: provisioner.DefaultWaitOptions(),
		}, nil

	case core.ProviderGCP:
		project, err := cmd.Flags().GetString("gcp-project")
		if err != nil {
			return orchestrator.Cloud{}, fmt.Errorf("reading --gcp-project: %w", err)
		}
		if project == "" {
			return orchestrator.Cloud{}, fmt.Errorf("%w: --gcp-project is required for provider gcp", core.ErrInvalidSpec)
		}

		clients, err := gcpprov.NewClients(ctx, project, gcpprov.WithLogger(logger))
		if err != nil {
			return orchestrator.Cloud{}, fmt.Errorf("building GCP clients: %w", err)
		}

		return orchestrator.Cloud{
			Cluster:  gcpprov.NewClusterProvisioner(clients),
			Identity: gcpprov.NewIdentityProvisioner(clients),
			Network:  gcpprov.NewNetworkProvisioner(clients),
			IngestionEndpoint: provisioner.EgressDestination{
				Host:        endpoint,
				Port:        443,
				Description: "kubespin fleet-status-reporter egress",
			},
			Wait: provisioner.DefaultWaitOptions(),
		}, nil

	case core.ProviderAzure:
		subscription, err := cmd.Flags().GetString("azure-subscription")
		if err != nil {
			return orchestrator.Cloud{}, fmt.Errorf("reading --azure-subscription: %w", err)
		}
		if subscription == "" {
			return orchestrator.Cloud{}, fmt.Errorf(
				"%w: --azure-subscription is required for provider azure", core.ErrInvalidSpec)
		}

		clients, err := azureprov.NewClients(subscription, azureprov.WithLogger(logger))
		if err != nil {
			return orchestrator.Cloud{}, fmt.Errorf("building Azure clients: %w", err)
		}

		return orchestrator.Cloud{
			Cluster:  azureprov.NewClusterProvisioner(clients),
			Identity: azureprov.NewIdentityProvisioner(clients),
			Network:  azureprov.NewNetworkProvisioner(clients),
			IngestionEndpoint: provisioner.EgressDestination{
				Host:        endpoint,
				Port:        443,
				Description: "kubespin fleet-status-reporter egress",
			},
			Wait: provisioner.DefaultWaitOptions(),
		}, nil

	default:
		return orchestrator.Cloud{}, fmt.Errorf("%w: unknown provider %q", core.ErrInvalidSpec, spec.Provider)
	}
}

// buildRepoClients assembles the GitHub clients every provider shares: a
// cluster's repository always lives on the same GitHub Enterprise instance
// regardless of which cloud the cluster itself runs on.
//
// The token comes from GITHUB_TOKEN rather than a flag: flag values land in
// shell history, and every other secret this CLI touches (cloud credentials)
// is already sourced from the ambient environment rather than a flag.
func buildRepoClients(cmd *cobra.Command) (*repo.Clients, error) {
	org, err := cmd.Flags().GetString("github-org")
	if err != nil {
		return nil, fmt.Errorf("reading --github-org: %w", err)
	}
	if org == "" {
		return nil, fmt.Errorf("%w: --github-org is required", core.ErrInvalidSpec)
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("%w: GITHUB_TOKEN must be set", core.ErrInvalidSpec)
	}

	baseURL, err := cmd.Flags().GetString("github-base-url")
	if err != nil {
		return nil, fmt.Errorf("reading --github-base-url: %w", err)
	}
	uploadURL, err := cmd.Flags().GetString("github-upload-url")
	if err != nil {
		return nil, fmt.Errorf("reading --github-upload-url: %w", err)
	}

	clients, err := repo.NewClients(org, baseURL, uploadURL, token)
	if err != nil {
		return nil, fmt.Errorf("building GitHub clients: %w", err)
	}
	return clients, nil
}

func newDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Decommission a cluster and its supporting resources",
		Long: `delete performs the teardown in reverse order: mark the cluster
decommissioning in the Fleet Registry, clean up identity and OIDC resources,
delete the cluster, archive its repository, and record it decommissioned.

Repositories are archived, never deleted: history is retained.

delete is idempotent and resumable exactly like apply: a cluster already
decommissioned is a no-op, and a failed teardown resumes from
decommissioning on retry rather than needing to be reasoned about by hand.

The spec identifies which cluster and cloud to tear down. It may come from a
cluster.yaml — the same file the cluster's repository holds — or from the
flags below, which override the file when given.

delete does not honour the global --dry-run flag: passing it does not make
this command a preview. Use --yes to skip the confirmation prompt only when
you mean it.`,
		Example: `  # AWS, prompts to type the cluster ID to confirm
  kubespin delete --provider aws --region us-east-1 --cluster-id demo-aws \
    --github-org GitOpsHub

  # GCP, scripted (no interactive confirmation)
  kubespin delete --provider gcp --gcp-project kubernetes-dev-502710 --region us-central1 \
    --cluster-id demo-gcp \
    --github-org GitOpsHub --yes

  # Using the same cluster.yaml apply was run with
  kubespin delete --spec ./cluster.yaml \
    --github-org GitOpsHub --yes`,
		Args: cobra.NoArgs,
		RunE: runDelete,
	}

	fs := cmd.Flags()
	fs.String("spec", "", "path to a cluster.yaml describing the cluster")
	fs.String("cluster-id", "", "cluster identifier")
	fs.String("provider", "", "cloud provider: aws, gcp, or azure")
	fs.String("region", "", "cloud region")
	fs.String("access", string(core.AccessPrivate), "API server exposure: private or public (must match the cluster's spec)")
	fs.String("size", "small", "unused by delete, kept for spec compatibility")
	fs.String("kubernetes-version", "", "Kubernetes minor version, e.g. 1.34 (unused by delete, kept for spec compatibility)")
	fs.StringSlice("subnets", nil, "existing subnets the cluster was placed in")
	fs.StringSlice("authorized-cidrs", nil, "unused by delete, kept for spec compatibility")
	fs.String("vpc-cidr", "", "unused by delete, kept for spec compatibility")
	fs.String("vnet-cidr", "", "unused by delete, kept for spec compatibility")
	fs.String("subnet-cidr", "", "unused by delete, kept for spec compatibility")
	fs.String("gcp-project", "", "GCP project hosting the cluster (required for --provider gcp)")
	fs.String("azure-subscription", "", "Azure subscription hosting the cluster (required for --provider azure)")
	fs.String("github-org", "", "GitHub organization the cluster repository lives in")
	fs.String("github-base-url", "", "GitHub Enterprise API base URL (leave empty for github.com)")
	fs.String("github-upload-url", "", "GitHub Enterprise upload URL (leave empty for github.com)")
	fs.String("instance-type", "m6i.large", "instance type for the default node pool (unused by delete, kept for spec compatibility)")
	fs.Int32("min-size", 1, "minimum size of the default node pool (unused by delete, kept for spec compatibility)")
	fs.Int32("max-size", 5, "maximum size of the default node pool (unused by delete, kept for spec compatibility)")
	fs.Int32("desired-size", 2, "desired size of the default node pool (unused by delete, kept for spec compatibility)")
	fs.Int32("disk-size", 0, "boot disk size in GB for the default node pool's nodes (unused by delete, kept for spec compatibility)")
	fs.Bool("spot", false, "unused by delete, kept for spec compatibility")
	fs.String("zone", "", "unused by delete, kept for spec compatibility")
	fs.Bool("gcp-public-nodes", false, "unused by delete, kept for spec compatibility")
	fs.Bool("yes", false, "skip the interactive confirmation prompt")

	return cmd
}

func runDelete(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	logger := LoggerFrom(ctx)

	spec, err := loadSpec(cmd)
	if err != nil {
		return err
	}

	_, reg, err := registryPrereqs(cmd)
	if err != nil {
		return err
	}

	if err := ensureAuthenticated(cmd, cloudAuthProviders(spec)...); err != nil {
		return err
	}

	confirmed, err := cmd.Flags().GetBool("yes")
	if err != nil {
		return fmt.Errorf("reading --yes: %w", err)
	}
	if !confirmed {
		ok, err := confirmDelete(cmd, spec)
		if err != nil {
			return err
		}
		if !ok {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "aborted")
			return nil
		}
	}

	cloud, err := buildCloud(ctx, cmd, spec)
	if err != nil {
		return err
	}

	repoClients, err := buildRepoClients(cmd)
	if err != nil {
		return err
	}
	repoProv := repo.NewProvisioner(repoClients, repo.WithLogger(logger))

	o := orchestrator.New(reg, orchestrator.WithLogger(logger))

	rec, err := o.Delete(ctx, spec, orchestrator.Teardown(cloud, repoProv, logger))
	if err != nil {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "cluster %s stopped at phase %s\n", spec.ID, rec.Phase)
		return fmt.Errorf("deleting %s: %w", spec.ID, err)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "cluster %s is %s\n", rec.ClusterID, rec.Phase)
	return nil
}

// confirmDelete prompts on stdin/stdout for the cluster ID before a
// destructive teardown proceeds. --yes skips this for scripted use.
//
// Anything that is not the cluster ID declines, including an empty line and
// EOF. Reading the line rather than scanning a whitespace-delimited token is
// what makes that true: pressing Enter at this prompt, or piping in nothing,
// is how a person backs out, and it used to fail the command with "unexpected
// newline" instead — an error report for what was really a successful abort.
func confirmDelete(cmd *cobra.Command, spec core.ClusterSpec) (bool, error) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "This will decommission cluster %s and delete its cloud resources. Type the cluster ID to confirm: ", spec.ID)

	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("reading confirmation: %w", err)
	}
	return strings.TrimSpace(line) == spec.ID.String(), nil
}
