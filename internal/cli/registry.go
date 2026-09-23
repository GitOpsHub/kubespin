package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/GitOpsHub/kubespin/internal/registry"
)

// registryPrereqs resolves the config and connects to the cluster registry —
// the two things every command that talks to the registry needs before it can
// do anything else, shared by apply and delete.
func registryPrereqs(cmd *cobra.Command) (*Config, registry.Registry, error) {
	ctx := cmd.Context()

	cfg, ok := ConfigFrom(ctx)
	if !ok {
		return nil, nil, errors.New("configuration was not resolved")
	}
	if cfg.Registry.DSN == "" {
		return nil, nil, fmt.Errorf("%w: the Postgres registry DSN is required (KUBESPIN_REGISTRY_DSN)", ErrConfig)
	}

	opts := []registry.Option{registry.WithLogger(LoggerFrom(ctx))}
	if cfg.DryRun {
		// A dry run is strictly read-only, schema included: no CREATE/ALTER
		// against the operator's database just to report a plan.
		opts = append(opts, registry.WithoutMigrations())
	}
	reg, err := registry.NewPostgres(ctx, cfg.Registry.DSN, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to the cluster registry: %w", err)
	}
	return cfg, reg, nil
}
