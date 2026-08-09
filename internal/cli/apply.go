package cli

import (
	"github.com/spf13/cobra"

	"github.com/GitOpsHub/kubespin/internal/core"
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
recorded in the Fleet Registry.`,
		Args: cobra.NoArgs,
		RunE: stub("apply"),
	}

	fs := cmd.Flags()
	fs.String("cluster-id", "", "cluster identifier (also the repository suffix)")
	fs.String("provider", "", "cloud provider: aws, gcp, or azure")
	fs.String("region", "", "cloud region")
	fs.String("access", string(core.AccessPrivate), "API server exposure: private or public")
	fs.String("profile", "", "profile reference from platform-profiles, e.g. tier-small@1.0.0")
	fs.String("spec", "", "path to a cluster.yaml, as an alternative to the flags above")

	return cmd
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
