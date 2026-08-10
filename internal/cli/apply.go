package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/GitOpsHub/kubespin/internal/argocd"
	"github.com/GitOpsHub/kubespin/internal/catalog"
	"github.com/GitOpsHub/kubespin/internal/core"
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
holds — or from the flags below, which override the file when given.`,
		Example: `  # AWS, private API server, default node pool
  ./bin/kubespin apply --provider aws --region us-east-1 --cluster-id demo-aws \
    --access private --profile tier-small@1.0.0 \
    --github-org GitOpsHub --registry-region us-east-1

  # GCP, public API server, larger node pool
  ./bin/kubespin apply --provider gcp --gcp-project my-gcp-project --region us-central1 \
    --cluster-id demo-gcp --access public --profile tier-small@1.0.0 \
    --instance-type e2-standard-4 --desired-size 3 \
    --github-org GitOpsHub --registry-region us-east-1

  # Azure, resolving addons from a platform-profiles repo instead of the builtin catalog
  ./bin/kubespin apply --provider azure --azure-subscription <subscription-id> --region eastus \
    --cluster-id demo-azure --access private --profile tier-standard@1.0.0 \
    --instance-type Standard_D4s_v7 \
    --profiles-repo platform-profiles \
    --github-org GitOpsHub --registry-region us-east-1

  # Preview what apply would do without touching any cloud
  ./bin/kubespin apply --spec ./cluster.yaml --registry-region us-east-1 --dry-run`,
		Args: cobra.NoArgs,
		RunE: runApply,
	}

	fs := cmd.Flags()
	fs.String("spec", "", "path to a cluster.yaml describing the cluster")
	fs.String("cluster-id", "", "cluster identifier (also the repository suffix)")
	fs.String("provider", "", "cloud provider: aws, gcp, or azure")
	fs.String("region", "", "cloud region")
	fs.String("access", string(core.AccessPrivate), "API server exposure: private or public")
	fs.String("profile", "", "profile reference from platform-profiles, e.g. tier-small@1.0.0")
	fs.String("kubernetes-version", "", "Kubernetes minor version, e.g. 1.34")
	fs.StringSlice("subnets", nil, "existing subnets to place the cluster in")
	fs.String("vpc-cidr", "", "address space for the VPC kubespin creates when --subnets is omitted (AWS only, default 10.0.0.0/16)")
	fs.String("vnet-cidr", "", "address space for the VNet kubespin creates when --subnets is omitted (Azure only, default 10.0.0.0/16)")
	fs.String("subnet-cidr", "", "address prefix for the subnet kubespin creates when --subnets is omitted (Azure default 10.0.1.0/24, GCP default 10.0.0.0/20)")
	fs.String("ingestion-endpoint", "", "Central Ingestion API host the cluster must be able to reach")
	fs.String("gcp-project", "", "GCP project hosting the cluster (required for --provider gcp)")
	fs.String("azure-subscription", "", "Azure subscription hosting the cluster (required for --provider azure)")
	fs.String("github-org", "", "GitHub organization cluster repositories are created in")
	fs.String("github-base-url", "", "GitHub Enterprise API base URL (leave empty for github.com)")
	fs.String("github-upload-url", "", "GitHub Enterprise upload URL (leave empty for github.com)")
	fs.String("profiles-repo", "", "platform-profiles repository name to resolve profiles from (uses the builtin catalog if empty)")

	fs.String("instance-type", "m6i.large", "instance type for the default node pool (defaults to a cloud-appropriate value per --provider when unset: m6i.large on aws, e2-standard-4 on gcp, Standard_D4s_v7 on azure)")
	fs.Int32("min-size", 1, "minimum size of the default node pool")
	fs.Int32("max-size", 5, "maximum size of the default node pool")
	fs.Int32("desired-size", 2, "desired size of the default node pool")
	fs.Int32("disk-size", 0, "boot disk size in GB for the default node pool's nodes (0 = cloud default; GKE regional clusters multiply this by the number of zones, so it is worth setting explicitly on quota-constrained projects)")

	return cmd
}

func runApply(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	logger := LoggerFrom(ctx)

	cfg, ok := ConfigFrom(ctx)
	if !ok {
		return errors.New("configuration was not resolved")
	}

	spec, err := loadSpec(cmd)
	if err != nil {
		return err
	}
	if cfg.Registry.Region == "" {
		return fmt.Errorf("%w: --registry-region is required", ErrConfig)
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

	reg, err := registry.NewDynamoDB(ctx, cfg.Registry.Region, cfg.Registry.Table, registry.WithLogger(logger))
	if err != nil {
		return fmt.Errorf("connecting to the Fleet Registry: %w", err)
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

	resolver, err := buildResolver(cmd, repoClients)
	if err != nil {
		return err
	}

	o := orchestrator.New(reg,
		orchestrator.WithSteps(orchestrator.ProvisioningSteps(
			cloud, repoProv, resolver, reg,
			argocd.NewHelmInstaller(logger), argocd.NewDynamicApplier(logger), logger,
		)),
		orchestrator.WithReadyReconcile(orchestrator.ReadyReconcile(cloud, repoProv, resolver, logger)),
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
	return nil
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

// buildRepoClients assembles the GitHub clients every provider shares:
// unlike ClusterProvisioner and IdentityProvisioner, a cluster's repository
// always lives on the same GitHub Enterprise instance regardless of which
// cloud the cluster itself runs on. runApply builds both the repository
// Provisioner and buildResolver's platform-profiles Resolver on top of this,
// so the platform-profiles repo (M4) is read through the same credentials the
// cluster's own repo (M3) is written through.
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

// buildResolver picks the profile catalog: the real platform-profiles-repo
// resolver when --profiles-repo names one, the builtin placeholder set
// otherwise. Falling back rather than requiring the flag keeps `apply`
// usable before that repo has been stood up (M4's first, still-open item).
func buildResolver(cmd *cobra.Command, clients *repo.Clients) (catalog.Resolver, error) {
	profilesRepo, err := cmd.Flags().GetString("profiles-repo")
	if err != nil {
		return nil, fmt.Errorf("reading --profiles-repo: %w", err)
	}
	if profilesRepo == "" {
		return catalog.NewBuiltinResolver(), nil
	}
	return catalog.NewRepoResolver(clients, profilesRepo), nil
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
  ./bin/kubespin delete --provider aws --region us-east-1 --cluster-id demo-aws \
    --profile tier-small@1.0.0 --github-org GitOpsHub --registry-region us-east-1

  # GCP, scripted (no interactive confirmation)
  ./bin/kubespin delete --provider gcp --gcp-project my-gcp-project --region us-central1 \
    --cluster-id demo-gcp --profile tier-small@1.0.0 \
    --github-org GitOpsHub --registry-region us-east-1 --yes

  # Using the same cluster.yaml apply was run with
  ./bin/kubespin delete --spec ./cluster.yaml \
    --github-org GitOpsHub --registry-region us-east-1 --yes`,
		Args: cobra.NoArgs,
		RunE: runDelete,
	}

	fs := cmd.Flags()
	fs.String("spec", "", "path to a cluster.yaml describing the cluster")
	fs.String("cluster-id", "", "cluster identifier")
	fs.String("provider", "", "cloud provider: aws, gcp, or azure")
	fs.String("region", "", "cloud region")
	fs.String("access", string(core.AccessPrivate), "API server exposure: private or public (must match the cluster's spec)")
	fs.String("profile", "", "profile reference from platform-profiles, e.g. tier-small@1.0.0")
	fs.String("kubernetes-version", "", "Kubernetes minor version, e.g. 1.34 (unused by delete, kept for spec compatibility)")
	fs.StringSlice("subnets", nil, "existing subnets the cluster was placed in")
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
	fs.Bool("yes", false, "skip the interactive confirmation prompt")

	return cmd
}

func runDelete(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	logger := LoggerFrom(ctx)

	cfg, ok := ConfigFrom(ctx)
	if !ok {
		return errors.New("configuration was not resolved")
	}

	spec, err := loadSpec(cmd)
	if err != nil {
		return err
	}
	if cfg.Registry.Region == "" {
		return fmt.Errorf("%w: --registry-region is required", ErrConfig)
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

	reg, err := registry.NewDynamoDB(ctx, cfg.Registry.Region, cfg.Registry.Table, registry.WithLogger(logger))
	if err != nil {
		return fmt.Errorf("connecting to the Fleet Registry: %w", err)
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

// confirmDelete prompts on stdin/stdout for an explicit "yes" before a
// destructive teardown proceeds. --yes skips this for scripted use.
func confirmDelete(cmd *cobra.Command, spec core.ClusterSpec) (bool, error) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "This will decommission cluster %s and delete its cloud resources. Type the cluster ID to confirm: ", spec.ID)

	var response string
	if _, err := fmt.Fscanln(cmd.InOrStdin(), &response); err != nil {
		return false, fmt.Errorf("reading confirmation: %w", err)
	}
	return response == spec.ID.String(), nil
}
