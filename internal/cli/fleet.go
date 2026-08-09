package cli

import (
	"github.com/spf13/cobra"
)

func newFleetCommand() *cobra.Command {
	fleet := &cobra.Command{
		Use:   "fleet",
		Short: "Operate on the whole fleet rather than a single cluster",
		Args:  cobra.NoArgs,
		// With no subcommand, print help rather than failing.
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	fleet.AddCommand(
		newFleetBootstrapCommand(),
		newFleetUpdateCommand(),
		newFleetAuditCommand(),
		newFleetStatusCommand(),
	)
	return fleet
}

func newFleetUpdateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Roll a component version across every matching cluster",
		Long: `update patches the repository of every cluster matching the given profile,
staged in waves through a rate-limited worker pool, canary tier first.`,
		Args: cobra.NoArgs,
		RunE: stub("fleet update"),
	}

	fs := cmd.Flags()
	fs.String("profile", "", "restrict to clusters on this profile")
	fs.String("component", "", "addon to update")
	fs.String("version", "", "target version")
	fs.Int("concurrency", 4, "maximum concurrent repository updates")

	return cmd
}

func newFleetAuditCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Diff live cloud infrastructure against each cluster's desired state",
		Long: `audit describes live infrastructure through each cloud's SDK, diffs it against
the cluster.yaml in that cluster's repository, and writes findings to the Fleet
Registry. It detects changes made outside kubespin, such as a manually resized
node pool.`,
		Args: cobra.NoArgs,
		RunE: stub("fleet audit"),
	}

	fs := cmd.Flags()
	fs.String("provider", "", "restrict to one cloud provider")
	fs.Int("concurrency", 4, "maximum concurrent cluster audits")

	return cmd
}

func newFleetStatusCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report sync, drift, and staleness across the fleet",
		Long: `status reads the Fleet Registry, which is populated by each cluster's
fleet-status-reporter pushing outward. It never connects to a cluster, so a
cluster that is unreachable shows as stale rather than blocking the command.`,
		Args: cobra.NoArgs,
		RunE: stub("fleet status"),
	}

	fs := cmd.Flags()
	fs.String("provider", "", "restrict to one cloud provider")
	fs.String("phase", "", "restrict to clusters in one phase")
	fs.Bool("stale-only", false, "show only clusters that have missed their reporting window")
	fs.String("output", "table", "output format: table or json")

	return cmd
}
