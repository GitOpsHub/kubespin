package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/orchestrator"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
	awsprov "github.com/GitOpsHub/kubespin/internal/provisioner/aws"
	"github.com/GitOpsHub/kubespin/internal/registry"
)

// ErrProviderNotImplemented marks a cloud whose provisioner has not landed yet.
var ErrProviderNotImplemented = errors.New("provider is not implemented yet")

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
	fs.String("ingestion-endpoint", "", "Central Ingestion API host the cluster must be able to reach")

	fs.String("instance-type", "m6i.large", "instance type for the default node pool")
	fs.Int32("min-size", 1, "minimum size of the default node pool")
	fs.Int32("max-size", 5, "maximum size of the default node pool")
	fs.Int32("desired-size", 2, "desired size of the default node pool")

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

	reg, err := registry.NewDynamoDB(ctx, cfg.Registry.Region, cfg.Registry.Table)
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

	o := orchestrator.New(reg,
		orchestrator.WithSteps(orchestrator.ProvisioningSteps(cloud, logger)),
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

// buildCloud assembles the provisioners for the spec's cloud.
func buildCloud(ctx context.Context, cmd *cobra.Command, spec core.ClusterSpec) (orchestrator.Cloud, error) {
	endpoint, err := cmd.Flags().GetString("ingestion-endpoint")
	if err != nil {
		return orchestrator.Cloud{}, fmt.Errorf("reading --ingestion-endpoint: %w", err)
	}

	switch spec.Provider {
	case core.ProviderAWS:
		clients, err := awsprov.NewClients(ctx, spec.Region)
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

	case core.ProviderGCP, core.ProviderAzure:
		// Deliberately explicit rather than a generic failure: the interfaces
		// exist and AWS proves them, but GKE and AKS land in the next slice of
		// M2.
		return orchestrator.Cloud{}, fmt.Errorf("%w: %s support lands with the rest of M2",
			ErrProviderNotImplemented, spec.Provider)

	default:
		return orchestrator.Cloud{}, fmt.Errorf("%w: unknown provider %q", core.ErrInvalidSpec, spec.Provider)
	}
}

func newDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Decommission a cluster and its supporting resources",
		Long: `delete performs the teardown in reverse order: mark the cluster
decommissioning in the Fleet Registry, clean up identity and OIDC resources,
delete the cluster, archive its repository, and record it decommissioned.

Repositories are archived, never deleted: history is retained.`,
		Args: cobra.NoArgs,
		RunE: stub("delete"),
	}

	fs := cmd.Flags()
	fs.String("cluster-id", "", "cluster identifier")
	fs.Bool("yes", false, "skip the interactive confirmation prompt")

	return cmd
}
