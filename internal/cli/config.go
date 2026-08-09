// Package cli wires the cobra command tree and resolves configuration.
package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// ErrConfig wraps every configuration resolution failure.
var ErrConfig = errors.New("configuration error")

// envPrefix namespaces environment variables: --log-level reads KUBESPIN_LOG_LEVEL.
const envPrefix = "KUBESPIN"

// Config is the resolved global configuration, assembled from (in decreasing
// precedence) command line flags, KUBESPIN_* environment variables, the config
// file, and built-in defaults.
type Config struct {
	LogLevel  string
	LogFormat string
	DryRun    bool

	// Registry addresses the Fleet Registry. Consumed from M1 onward.
	Registry RegistryConfig

	// SourceFile records which config file was loaded, if any. Empty when the
	// run was configured entirely by flags, env, and defaults.
	SourceFile string
}

// RegistryConfig locates the DynamoDB-backed Fleet Registry.
type RegistryConfig struct {
	Table  string
	Region string
}

// Defaults applied when nothing else supplies a value.
const (
	defaultLogLevel      = "info"
	defaultLogFormat     = "text"
	defaultRegistryTable = "kubespin-fleet-registry"
)

// registerGlobalFlags declares the flags available on every command. It takes a
// FlagSet rather than a *cobra.Command so tests can exercise precedence against
// a bare flag set without building the whole command tree.
func registerGlobalFlags(fs *pflag.FlagSet) {
	fs.String("config", "", "path to config file (default: $XDG_CONFIG_HOME/kubespin/config.yaml)")
	fs.String("log-level", defaultLogLevel, "log verbosity: debug, info, warn, error")
	fs.String("log-format", defaultLogFormat, "log output format: text or json")
	fs.Bool("dry-run", false, "resolve and report intended changes without performing them")
	fs.String("registry-table", defaultRegistryTable, "DynamoDB table backing the Fleet Registry")
	fs.String("registry-region", "", "AWS region hosting the Fleet Registry")
}

// LoadConfig resolves configuration from fs plus environment and config file.
//
// Precedence is flags > KUBESPIN_* env > config file > defaults. Viper gives us
// this for free as long as flags are bound rather than read directly: an
// unchanged flag falls through to env and file, and its default is consulted
// only last.
func LoadConfig(fs *pflag.FlagSet) (*Config, error) {
	v := viper.New()

	v.SetEnvPrefix(envPrefix)
	// --log-level -> KUBESPIN_LOG_LEVEL, registry.table -> KUBESPIN_REGISTRY_TABLE.
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	v.AutomaticEnv()

	if err := v.BindPFlags(fs); err != nil {
		return nil, fmt.Errorf("%w: binding flags: %w", ErrConfig, err)
	}

	if err := loadConfigFile(v, fs); err != nil {
		return nil, err
	}

	cfg := &Config{
		LogLevel:   v.GetString("log-level"),
		LogFormat:  v.GetString("log-format"),
		DryRun:     v.GetBool("dry-run"),
		SourceFile: v.ConfigFileUsed(),
		Registry: RegistryConfig{
			Table:  v.GetString("registry-table"),
			Region: v.GetString("registry-region"),
		},
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loadConfigFile reads an explicit --config path, or searches the default
// locations. A missing explicit path is an error; a missing default one is not.
func loadConfigFile(v *viper.Viper, fs *pflag.FlagSet) error {
	explicit, err := fs.GetString("config")
	if err != nil {
		return fmt.Errorf("%w: reading --config: %w", ErrConfig, err)
	}
	if explicit == "" {
		explicit = os.Getenv(envPrefix + "_CONFIG")
	}

	if explicit != "" {
		v.SetConfigFile(explicit)
		if err := v.ReadInConfig(); err != nil {
			return fmt.Errorf("%w: reading config file %s: %w", ErrConfig, explicit, err)
		}
		return nil
	}

	v.SetConfigName("config")
	v.SetConfigType("yaml")
	if dir, err := os.UserConfigDir(); err == nil {
		v.AddConfigPath(filepath.Join(dir, "kubespin"))
	}
	v.AddConfigPath(".")

	var notFound viper.ConfigFileNotFoundError
	if err := v.ReadInConfig(); err != nil && !errors.As(err, &notFound) {
		return fmt.Errorf("%w: reading config file: %w", ErrConfig, err)
	}
	return nil
}

func (c *Config) validate() error {
	if _, err := parseLogLevel(c.LogLevel); err != nil {
		return err
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("%w: log-format %q must be text or json", ErrConfig, c.LogFormat)
	}
	return nil
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("%w: log-level %q must be debug, info, warn, or error", ErrConfig, s)
	}
}

// Logger builds the structured logger described by the config. Logs go to
// stderr so command output on stdout stays pipeable.
func (c *Config) Logger(w *os.File) *slog.Logger {
	level, err := parseLogLevel(c.LogLevel)
	if err != nil {
		level = slog.LevelInfo // validate() ran first; this is belt and braces.
	}

	opts := &slog.HandlerOptions{Level: level}
	if c.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
