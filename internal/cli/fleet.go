package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/fleet"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
	"github.com/GitOpsHub/kubespin/internal/registry"
	"github.com/GitOpsHub/kubespin/internal/repo"
)

func newFleetCommand() *cobra.Command {
	fleetCmd := &cobra.Command{
		Use:   "fleet",
		Short: "Operate on the whole fleet rather than a single cluster",
		Example: `  # The typical fleet lifecycle, in order. KUBESPIN_REGISTRY_DSN must be set
  # (in .env or the environment) for every one of these.
  kubespin fleet bootstrap --account-id 465532803838 --region us-east-1
  kubespin fleet status
  kubespin fleet update --component argo-cd --version 2.11.0 --github-org GitOpsHub
  kubespin fleet audit --github-org GitOpsHub`,
		Args: cobra.NoArgs,
		// With no subcommand, print help rather than failing.
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	fleetCmd.AddCommand(
		newFleetBootstrapCommand(),
		newFleetUpdateCommand(),
		newFleetAuditCommand(),
		newFleetStatusCommand(),
		newFleetDashboardCommand(),
	)
	return fleetCmd
}

func newFleetUpdateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Roll a component version across every matching cluster",
		Long: `update patches the repository of every cluster matching the given profile,
staged through a rate-limited worker pool.

With --canary-count set, the first N matching clusters (ordered
deterministically by cluster ID, so a wave is reproducible run to run) are
updated first, as a canary wave. If any canary cluster's update fails, the
rest of the fleet is left untouched and reported "skipped" — canarying exists
to catch a bad version before it reaches every cluster, so a canary failure
must stop the rollout rather than continue past it. Only a clean canary wave
rolls to the rest, in a second wave. --canary-count 0 (the default) skips
canarying and updates every matching cluster in one wave.

--provider is the only filter that currently narrows a wave.

update does not honour the global --dry-run flag: a run commits to every
matching cluster's repository. A cluster already at the target version
reports "already up to date" and commits nothing, so re-running a partially
failed wave is safe.`,
		Example: `  # Roll a new Argo CD version across every cluster, 8 at a time
  kubespin fleet update --component argo-cd --version 2.11.0 --concurrency 8 \
    --github-org GitOpsHub

  # Canary the first 3 clusters before rolling to the rest of the fleet
  kubespin fleet update --component cert-manager --version 1.15.1 \
    --canary-count 3 --github-org GitOpsHub

  # Scope the wave to one cloud
  kubespin fleet update --component cert-manager --version 1.15.1 \
    --provider aws --github-org GitOpsHub`,
		Args: cobra.NoArgs,
		RunE: runFleetUpdate,
	}

	fs := cmd.Flags()
	fs.String("component", "", "addon to update")
	fs.String("version", "", "target version")
	fs.Int("concurrency", 4, "maximum concurrent repository updates")
	fs.Int("canary-count", 0, "update this many clusters first and abort before the rest of the fleet if any fail (0 disables canarying)")
	fs.String("provider", "", "restrict to one cloud provider")
	fs.String("github-org", "", "GitHub organization cluster repositories live in")
	fs.String("github-base-url", "", "GitHub Enterprise API base URL (leave empty for github.com)")
	fs.String("github-upload-url", "", "GitHub Enterprise upload URL (leave empty for github.com)")
	fs.String("profiles-repo", "", "platform-profiles repository name to resolve profiles from (uses the builtin catalog if empty)")

	return cmd
}

func runFleetUpdate(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	cfg, reg, err := registryPrereqs(cmd)
	if err != nil {
		return err
	}

	component, err := cmd.Flags().GetString("component")
	if err != nil {
		return fmt.Errorf("reading --component: %w", err)
	}
	version, err := cmd.Flags().GetString("version")
	if err != nil {
		return fmt.Errorf("reading --version: %w", err)
	}
	if component == "" || version == "" {
		return fmt.Errorf("%w: --component and --version are required", core.ErrInvalidSpec)
	}

	filter, err := fleetFilter(cmd, "")
	if err != nil {
		return err
	}

	repoClients, err := buildRepoClients(cmd)
	if err != nil {
		return err
	}
	repoProv := repo.NewProvisioner(repoClients, repo.WithLogger(LoggerFrom(ctx)))

	resolver, err := buildResolver(cmd, repoClients)
	if err != nil {
		return err
	}

	concurrency, err := cmd.Flags().GetInt("concurrency")
	if err != nil {
		return fmt.Errorf("reading --concurrency: %w", err)
	}
	canaryCount, err := cmd.Flags().GetInt("canary-count")
	if err != nil {
		return fmt.Errorf("reading --canary-count: %w", err)
	}

	results, err := fleet.Update(ctx, reg, filter, resolver, repoProv, component, version, concurrency, canaryCount,
		fleet.WithLogger(LoggerFrom(ctx)))
	if err != nil {
		return fmt.Errorf("running fleet update: %w", err)
	}

	return reportUpdateResults(cmd, results, cfg)
}

func reportUpdateResults(cmd *cobra.Command, results []fleet.UpdateResult, _ *Config) error {
	out := cmd.OutOrStdout()

	var failed, skipped int
	for _, r := range results {
		switch {
		case r.Skipped:
			skipped++
			_, _ = fmt.Fprintf(out, "%s: skipped (canary wave failed)\n", r.ClusterID)
		case r.Err != nil:
			failed++
			_, _ = fmt.Fprintf(out, "%s: FAILED: %v\n", r.ClusterID, r.Err)
		case r.Committed:
			_, _ = fmt.Fprintf(out, "%s: updated\n", r.ClusterID)
		default:
			_, _ = fmt.Fprintf(out, "%s: already up to date\n", r.ClusterID)
		}
	}
	_, _ = fmt.Fprintf(out, "%d cluster(s), %d failed, %d skipped\n", len(results), failed, skipped)

	if failed > 0 {
		return fmt.Errorf("fleet update: %d of %d clusters failed", failed, len(results))
	}
	return nil
}

func newFleetAuditCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Diff live cloud infrastructure against each cluster's desired state",
		Long: `audit describes live infrastructure through each cloud's SDK, diffs it against
the cluster.yaml in that cluster's repository, and reports findings. It
detects changes made outside kubespin, such as a manually resized node pool.

audit is read-only: it never reconciles or commits infrastructure or a
cluster's repository. It does write one thing: each cluster's findings (or a
clean result) are persisted to the Fleet Registry, so 'fleet status' and
other fleet-wide tooling can read the most recent audit result without
re-running one.`,
		Example: `  # Audit every cluster in the fleet
  kubespin fleet audit --github-org GitOpsHub

  # Audit only AWS clusters, with more concurrency
  kubespin fleet audit --provider aws --concurrency 8 \
    --github-org GitOpsHub

  # A fleet with GCP or Azure clusters needs their project/subscription too
  kubespin fleet audit --gcp-project kubernetes-dev-502710 \
    --azure-subscription 3df9adbd-ea55-4c92-964c-0252031979de \
    --github-org GitOpsHub`,
		Args: cobra.NoArgs,
		RunE: runFleetAudit,
	}

	fs := cmd.Flags()
	fs.String("provider", "", "restrict to one cloud provider")
	fs.Int("concurrency", 4, "maximum concurrent cluster audits")
	fs.String("gcp-project", "", "GCP project hosting any GCP clusters in the fleet")
	fs.String("azure-subscription", "", "Azure subscription hosting any Azure clusters in the fleet")
	fs.String("github-org", "", "GitHub organization cluster repositories live in")
	fs.String("github-base-url", "", "GitHub Enterprise API base URL (leave empty for github.com)")
	fs.String("github-upload-url", "", "GitHub Enterprise upload URL (leave empty for github.com)")

	return cmd
}

func runFleetAudit(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	_, reg, err := registryPrereqs(cmd)
	if err != nil {
		return err
	}

	filter, err := fleetFilter(cmd, "provider")
	if err != nil {
		return err
	}

	repoClients, err := buildRepoClients(cmd)
	if err != nil {
		return err
	}
	repoProv := repo.NewProvisioner(repoClients, repo.WithLogger(LoggerFrom(ctx)))

	concurrency, err := cmd.Flags().GetInt("concurrency")
	if err != nil {
		return fmt.Errorf("reading --concurrency: %w", err)
	}

	results, err := fleet.Audit(ctx, reg, filter, clusterProvisionerFactory(cmd), repoProv, concurrency,
		fleet.WithLogger(LoggerFrom(ctx)))
	if err != nil {
		return fmt.Errorf("running fleet audit: %w", err)
	}

	return reportAuditResults(cmd, results)
}

func reportAuditResults(cmd *cobra.Command, results []fleet.AuditResult) error {
	out := cmd.OutOrStdout()

	var failed, drifted int
	for _, r := range results {
		switch {
		case r.Err != nil:
			failed++
			_, _ = fmt.Fprintf(out, "%s: FAILED: %v\n", r.ClusterID, r.Err)
		case len(r.Findings) > 0:
			drifted++
			for _, f := range r.Findings {
				_, _ = fmt.Fprintf(out, "%s: %s\n", f.ClusterID, f.Detail)
			}
		default:
			_, _ = fmt.Fprintf(out, "%s: no drift\n", r.ClusterID)
		}
	}
	_, _ = fmt.Fprintf(out, "%d cluster(s), %d drifted, %d failed\n", len(results), drifted, failed)

	if failed > 0 {
		return fmt.Errorf("fleet audit: %d of %d clusters could not be audited", failed, len(results))
	}
	return nil
}

// clusterProvisionerFactory adapts buildCloud (which apply/delete use to
// build a full Cloud) into the narrower ClusterProvisioner-only factory
// fleet.Audit needs: an audit never binds identity or opens egress, so
// building the rest of Cloud for it would be wasted real cloud calls (an
// IRSA/Workload-Identity/federated-credential setup call per cluster,
// audited or not).
func clusterProvisionerFactory(cmd *cobra.Command) fleet.ClusterProvisionerFactory {
	return func(ctx context.Context, prov core.Provider, region string) (provisioner.ClusterProvisioner, error) {
		cloud, err := buildCloud(ctx, cmd, core.ClusterSpec{Provider: prov, Region: region})
		if err != nil {
			return nil, err
		}
		return cloud.Cluster, nil
	}
}

func newFleetStatusCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report sync, drift, and staleness across the fleet",
		Long: `status reads the Fleet Registry, which is populated by each cluster's
fleet-status-reporter pushing outward. It never connects to a cluster, so a
cluster that is unreachable shows as stale rather than blocking the command.`,
		Example: `  # Every cluster, as a table
  kubespin fleet status

  # Only clusters that have missed their reporting window
  kubespin fleet status --stale-only --stale-threshold 30m

  # Machine-readable output, restricted to one phase
  kubespin fleet status --phase ready --output json`,
		Args: cobra.NoArgs,
		RunE: runFleetStatus,
	}

	fs := cmd.Flags()
	fs.String("provider", "", "restrict to one cloud provider")
	fs.String("phase", "", "restrict to clusters in one phase")
	fs.Bool("stale-only", false, "show only clusters that have missed their reporting window")
	fs.String("output", "table", "output format: table or json")
	fs.Duration("stale-threshold", fleet.DefaultStaleThreshold, "how long a cluster may go without reporting before it is stale")

	return cmd
}

func runFleetStatus(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	_, reg, err := registryPrereqs(cmd)
	if err != nil {
		return err
	}

	filter, err := fleetFilter(cmd, "provider")
	if err != nil {
		return err
	}

	phase, err := cmd.Flags().GetString("phase")
	if err != nil {
		return fmt.Errorf("reading --phase: %w", err)
	}
	filter.Phase = core.Phase(phase)

	staleOnly, err := cmd.Flags().GetBool("stale-only")
	if err != nil {
		return fmt.Errorf("reading --stale-only: %w", err)
	}
	threshold, err := cmd.Flags().GetDuration("stale-threshold")
	if err != nil {
		return fmt.Errorf("reading --stale-threshold: %w", err)
	}

	statuses, err := fleet.Status(ctx, reg, filter, staleOnly, threshold, time.Now(),
		fleet.WithLogger(LoggerFrom(ctx)))
	if err != nil {
		return fmt.Errorf("running fleet status: %w", err)
	}

	output, err := cmd.Flags().GetString("output")
	if err != nil {
		return fmt.Errorf("reading --output: %w", err)
	}
	return reportStatuses(cmd, statuses, output)
}

func reportStatuses(cmd *cobra.Command, statuses []fleet.ClusterStatus, output string) error {
	out := cmd.OutOrStdout()

	switch output {
	case "json":
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(statuses); err != nil {
			return fmt.Errorf("encoding status as json: %w", err)
		}
		return nil
	case "table", "":
		_, _ = fmt.Fprintf(out, "%-30s %-8s %-18s %-6s %-9s %s\n",
			"CLUSTER", "PROVIDER", "PHASE", "STALE", "DRIFT", "LAST REPORTED")
		for _, s := range statuses {
			last := "never"
			if !s.LastReportedAt.IsZero() {
				last = s.LastReportedAt.Format(time.RFC3339)
			}
			drift := "never audited"
			if !s.FindingsAt.IsZero() {
				drift = fmt.Sprintf("%d findings", s.FindingsCount)
			}
			_, _ = fmt.Fprintf(out, "%-30s %-8s %-18s %-6t %-9s %s\n",
				s.ClusterID, s.Provider, s.Phase, s.Stale, drift, last)
		}
		return nil
	default:
		return fmt.Errorf("%w: --output must be table or json", core.ErrInvalidSpec)
	}
}

// registryPrereqs resolves the config and connects to the Fleet Registry —
// the two things every command that talks to the registry needs before it
// can do anything else, shared by the fleet subcommands and by apply/delete.
func registryPrereqs(cmd *cobra.Command) (*Config, registry.Registry, error) {
	ctx := cmd.Context()

	cfg, ok := ConfigFrom(ctx)
	if !ok {
		return nil, nil, errors.New("configuration was not resolved")
	}
	if cfg.Registry.DSN == "" {
		return nil, nil, fmt.Errorf("%w: the Postgres registry DSN is required (KUBESPIN_REGISTRY_DSN)", ErrConfig)
	}

	reg, err := registry.NewPostgres(ctx, cfg.Registry.DSN, registry.WithLogger(LoggerFrom(ctx)))
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to the Fleet Registry: %w", err)
	}
	return cfg, reg, nil
}

// fleetFilter builds a registry.Filter from --provider, when the command
// defines it under flagName (some commands don't take a --provider flag at
// all, in which case flagName is empty and the filter matches every
// provider).
func fleetFilter(cmd *cobra.Command, flagName string) (registry.Filter, error) {
	if flagName == "" {
		flagName = "provider"
	}
	if cmd.Flags().Lookup(flagName) == nil {
		return registry.Filter{}, nil
	}

	provider, err := cmd.Flags().GetString(flagName)
	if err != nil {
		return registry.Filter{}, fmt.Errorf("reading --%s: %w", flagName, err)
	}
	return registry.Filter{Provider: core.Provider(provider)}, nil
}
