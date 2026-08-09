package cli

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/GitOpsHub/kubespin/internal/auth"
)

func newLoginCommand() *cobra.Command {
	var only []string
	var force bool

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate to every configured cloud provider",
		Long: `login authenticates to every cloud provider kubespin talks to — AWS, GCP,
and Azure — skipping any provider whose session already looks valid.

Logins run concurrently: each provider may open a browser, and there is no
dependency between them, so waiting for them one at a time would just be a
needless delay.`,
		Example: `  # Log in to every configured provider
  ./bin/kubespin login

  # Only AWS and GCP
  ./bin/kubespin login --only aws,gcp

  # Re-authenticate even if the session still looks valid
  ./bin/kubespin login --force`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := LoggerFrom(cmd.Context())

			providers, err := selectAuthProviders(cmd, only)
			if err != nil {
				return err
			}

			logger.Info("logging in", "providers", providerNameList(providers), "force", force)
			results := auth.Login(cmd.Context(), providers, force)
			logResults(logger, results)
			auth.WriteTable(cmd.OutOrStdout(), results)
			return firstAuthError(results)
		},
	}

	cmd.Flags().StringSliceVar(&only, "only", nil, "comma-separated providers to log in to, e.g. aws,gcp (default: all)")
	cmd.Flags().BoolVar(&force, "force", false, "re-authenticate even if the session already looks valid")
	return cmd
}

func newStatusCommand() *cobra.Command {
	var only []string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show authentication state per cloud provider",
		Long: `status is read-only: it reports whether each provider's session currently
looks valid, without logging in, logging out, or otherwise changing anything.

Use this to debug "why is my provisioner failing" before assuming the bug is
in kubespin rather than an expired session.`,
		Example: `  # Every configured provider
  ./bin/kubespin status

  # Just Azure
  ./bin/kubespin status --only azure`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := LoggerFrom(cmd.Context())

			providers, err := selectAuthProviders(cmd, only)
			if err != nil {
				return err
			}

			logger.Debug("checking authentication status", "providers", providerNameList(providers))
			results := auth.Status(cmd.Context(), providers)
			logResults(logger, results)
			auth.WriteTable(cmd.OutOrStdout(), results)
			// status never fails the command on an unauthenticated provider —
			// that is exactly what it exists to report, not an error condition.
			return nil
		},
	}

	cmd.Flags().StringSliceVar(&only, "only", nil, "comma-separated providers to check, e.g. aws,gcp (default: all)")
	return cmd
}

func newLogoutCommand() *cobra.Command {
	var only []string

	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Clear cached sessions for one or more cloud providers",
		Example: `  # Log out of every provider
  ./bin/kubespin logout

  # Just GCP
  ./bin/kubespin logout --only gcp`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := LoggerFrom(cmd.Context())

			providers, err := selectAuthProviders(cmd, only)
			if err != nil {
				return err
			}

			logger.Info("logging out", "providers", providerNameList(providers))
			results := auth.Logout(cmd.Context(), providers)
			logResults(logger, results)
			auth.WriteTable(cmd.OutOrStdout(), results)
			return firstAuthError(results)
		},
	}

	cmd.Flags().StringSliceVar(&only, "only", nil, "comma-separated providers to log out of, e.g. aws,gcp (default: all)")
	return cmd
}

// selectAuthProviders builds the auth registry and applies --only. It is
// shared by login/status/logout so the three commands can never drift on
// which providers exist or how --only is parsed.
func selectAuthProviders(cmd *cobra.Command, only []string) ([]auth.Provider, error) {
	reg, err := buildAuthRegistry(cmd)
	if err != nil {
		return nil, err
	}
	providers, err := reg.Select(only)
	if err != nil {
		return nil, fmt.Errorf("--only: %w", err)
	}
	return providers, nil
}

// ensureAuthenticated is the preflight every command that calls a cloud SDK
// should run before doing anything else: fail fast with "run kubespin login"
// rather than a cryptic SDK auth error partway through provisioning.
func ensureAuthenticated(cmd *cobra.Command, providerNames ...string) error {
	providers, err := selectAuthProviders(cmd, providerNames)
	if err != nil {
		return err
	}
	if err := auth.EnsureAll(cmd.Context(), providers); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}

// buildAuthRegistry constructs every configured provider, in the order
// login/status/logout report them.
func buildAuthRegistry(cmd *cobra.Command) (*auth.Registry, error) {
	awsProvider, err := auth.NewAWSProvider(cmd.Context(), "default")
	if err != nil {
		return nil, fmt.Errorf("building AWS auth provider: %w", err)
	}
	return auth.NewRegistry(awsProvider, auth.NewGCPProvider(), auth.NewAzureProvider()), nil
}

// firstAuthError surfaces the first per-provider failure as the command's
// exit status, after the table has already shown every provider's outcome.
func firstAuthError(results []auth.Result) error {
	for _, r := range results {
		if r.Err != nil {
			return fmt.Errorf("%s: %w", r.Provider, r.Err)
		}
	}
	return nil
}

// providerNameList extracts provider names for a single "providers" log
// attribute, rather than logging the Provider values themselves.
func providerNameList(providers []auth.Provider) []string {
	names := make([]string, len(providers))
	for i, p := range providers {
		names[i] = p.Name()
	}
	return names
}

// logResults emits one structured log line per provider outcome, at debug
// level on success and warn on failure — the table printed to stdout is for
// the operator, this is for anyone piping --log-format json into a log
// aggregator.
func logResults(logger *slog.Logger, results []auth.Result) {
	for _, r := range results {
		if r.Err != nil {
			logger.Warn("provider auth check failed", "provider", r.Provider, "error", r.Err)
			continue
		}
		logger.Debug("provider auth result",
			"provider", r.Provider,
			"authenticated", r.Authenticated,
			"detail", r.Status.Message,
		)
	}
}
