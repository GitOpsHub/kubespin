package cli

import (
	"context"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/GitOpsHub/kubespin/internal/version"
)

type contextKey struct{ name string }

var (
	configKey = contextKey{"config"}
	loggerKey = contextKey{"logger"}
)

// NewRootCommand builds the full command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "kubespin",
		Short: "Provision and manage Kubernetes clusters across EKS, GKE, and AKS",
		Long: `kubespin provisions Kubernetes clusters across AWS, GCP, and Azure, each with
its own repository and its own local Argo CD instance syncing from it.

Clusters are never reached inbound: status flows outward from an in-cluster
reporter to the Fleet Registry.`,
		Version:       version.String(),
		SilenceUsage:  true, // usage text on a runtime error is noise
		SilenceErrors: true, // main formats errors itself
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := LoadConfig(cmd.Root().PersistentFlags())
			if err != nil {
				return err
			}

			logger := cfg.Logger(os.Stderr)
			if cfg.SourceFile != "" {
				logger.Debug("loaded config file", "path", cfg.SourceFile)
			}
			if cfg.DryRun {
				logger.Info("dry run: no changes will be made")
			}

			ctx := context.WithValue(cmd.Context(), configKey, cfg)
			ctx = context.WithValue(ctx, loggerKey, logger)
			cmd.SetContext(ctx)
			return nil
		},
	}

	registerGlobalFlags(root.PersistentFlags())
	root.SetVersionTemplate("{{.Version}}\n")

	root.AddCommand(
		newApplyCommand(),
		newDeleteCommand(),
		newFleetCommand(),
		newLoginCommand(),
		newStatusCommand(),
		newLogoutCommand(),
	)
	return root
}

// ConfigFrom returns the resolved config carried on ctx.
func ConfigFrom(ctx context.Context) (*Config, bool) {
	cfg, ok := ctx.Value(configKey).(*Config)
	return cfg, ok
}

// LoggerFrom returns the logger carried on ctx, falling back to the default so
// callers never have to nil-check.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
