package cli

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/spf13/cobra"

	"github.com/GitOpsHub/kubespin/internal/fleetinfra"
)

// defaultLambdaBinary is where `make lambda` puts the compiled handler.
const defaultLambdaBinary = "bin/ingestion/bootstrap"

func newFleetBootstrapCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Provision the shared fleet infrastructure in the fleet account",
		Long: `bootstrap creates the Fleet Registry table and the Central Ingestion API,
converging live infrastructure toward the desired state.

It is safe to re-run: every resource is create-or-update, and a run against
already-provisioned infrastructure reports no changes. Nothing is ever deleted.

This provisions shared platform infrastructure and must be run against a
dedicated fleet account that hosts no clusters. The caller's real account is
checked against --account-id before anything is created.`,
		Example: `  # Build the ingestion handler first: it is read from disk, not embedded
  make lambda

  # Preview what bootstrap would create
  ./bin/kubespin fleet bootstrap --account-id 111122223333 --registry-region us-east-1 --dry-run

  # Provision it for real
  ./bin/kubespin fleet bootstrap --account-id 111122223333 --registry-region us-east-1

  # Re-running is safe; a converged fleet reports no changes
  ./bin/kubespin fleet bootstrap --account-id 111122223333 --registry-region us-east-1 --dry-run`,
		Args: cobra.NoArgs,
		RunE: runFleetBootstrap,
	}

	fs := cmd.Flags()
	fs.String("account-id", "", "AWS account ID hosting fleet infrastructure (required)")
	fs.String("lambda-binary", defaultLambdaBinary, "compiled ingestion handler to deploy")
	fs.String("name-prefix", fleetinfra.DefaultNamePrefix, "prefix for every provisioned resource name")
	fs.Int32("log-retention-days", fleetinfra.DefaultLogRetentionDays, "CloudWatch log retention")
	fs.Int32("throttle-burst", fleetinfra.DefaultThrottleBurst, "ingestion API burst limit")
	fs.Float64("throttle-rate", fleetinfra.DefaultThrottleRate, "ingestion API steady-state request rate")

	if err := cmd.MarkFlagRequired("account-id"); err != nil {
		panic(err) // programmer error: the flag was just declared
	}
	return cmd
}

func runFleetBootstrap(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	logger := LoggerFrom(ctx)

	cfg, ok := ConfigFrom(ctx)
	if !ok {
		return errors.New("configuration was not resolved")
	}
	if cfg.Registry.Region == "" {
		return fmt.Errorf("%w: --registry-region is required to bootstrap", ErrConfig)
	}

	spec, err := bootstrapSpec(cmd, cfg)
	if err != nil {
		return err
	}

	clients, err := fleetinfra.NewClients(ctx, cfg.Registry.Region)
	if err != nil {
		return fmt.Errorf("building AWS clients: %w", err)
	}

	logger.Info("converging fleet infrastructure",
		"account", spec.AccountID, "region", spec.Region, "dry_run", cfg.DryRun)

	// The report is printed even on failure: a partial run has already created
	// resources, and the operator needs to see which.
	report, err := fleetinfra.Converge(ctx, clients, spec, cfg.DryRun, fleetinfra.WithLogger(logger))
	printReport(cmd, report)
	if err != nil {
		return fmt.Errorf("converging fleet infrastructure: %w", err)
	}
	return nil
}

func bootstrapSpec(cmd *cobra.Command, cfg *Config) (fleetinfra.Spec, error) {
	flags := cmd.Flags()

	accountID, err := flags.GetString("account-id")
	if err != nil {
		return fleetinfra.Spec{}, fmt.Errorf("reading --account-id: %w", err)
	}
	binaryPath, err := flags.GetString("lambda-binary")
	if err != nil {
		return fleetinfra.Spec{}, fmt.Errorf("reading --lambda-binary: %w", err)
	}
	namePrefix, err := flags.GetString("name-prefix")
	if err != nil {
		return fleetinfra.Spec{}, fmt.Errorf("reading --name-prefix: %w", err)
	}
	retention, err := flags.GetInt32("log-retention-days")
	if err != nil {
		return fleetinfra.Spec{}, fmt.Errorf("reading --log-retention-days: %w", err)
	}
	burst, err := flags.GetInt32("throttle-burst")
	if err != nil {
		return fleetinfra.Spec{}, fmt.Errorf("reading --throttle-burst: %w", err)
	}
	rate, err := flags.GetFloat64("throttle-rate")
	if err != nil {
		return fleetinfra.Spec{}, fmt.Errorf("reading --throttle-rate: %w", err)
	}

	// The handler is read from disk rather than embedded, so `go build ./...`
	// never depends on build ordering. Point at the exact fix when it is absent.
	zip, err := fleetinfra.PackageLambda(binaryPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fleetinfra.Spec{}, fmt.Errorf(
				"ingestion handler not found at %s: build it first with `make lambda`", binaryPath)
		}
		return fleetinfra.Spec{}, fmt.Errorf("packaging ingestion handler: %w", err)
	}

	return fleetinfra.Spec{
		AccountID:        accountID,
		Region:           cfg.Registry.Region,
		NamePrefix:       namePrefix,
		RegistryTable:    cfg.Registry.Table,
		LogRetentionDays: retention,
		ThrottleBurst:    burst,
		ThrottleRate:     rate,
		LambdaZip:        zip,
	}, nil
}

func printReport(cmd *cobra.Command, report fleetinfra.Report) {
	out := cmd.OutOrStdout()
	if len(report.Actions) == 0 {
		return
	}

	for _, action := range report.Actions {
		_, _ = fmt.Fprintln(out, "  "+action.String())
	}

	switch {
	case report.DryRun && report.Changed() > 0:
		_, _ = fmt.Fprintf(out, "\ndry run: %d resource(s) would change; re-run without --dry-run to apply\n",
			report.Changed())
	case report.DryRun:
		_, _ = fmt.Fprintln(out, "\ndry run: everything is in sync")
	case report.Changed() > 0:
		_, _ = fmt.Fprintf(out, "\nconverged: %d resource(s) changed\n", report.Changed())
	default:
		_, _ = fmt.Fprintln(out, "\nconverged: everything was already in sync")
	}

	if report.IngestionURL != "" {
		_, _ = fmt.Fprintf(out, "\ningestion endpoint: %s\n", report.IngestionURL)
		_, _ = fmt.Fprintln(out, "every cluster's egress allowlist must permit this host")
	}
}
