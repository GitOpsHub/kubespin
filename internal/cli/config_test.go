package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
)

// newFlags builds a flag set carrying the global flags and parses args against
// it, the same way cobra does for the root command's persistent flags.
func newFlags(t *testing.T, args ...string) *pflag.FlagSet {
	t.Helper()

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	registerGlobalFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}
	return fs
}

// writeConfig writes a config file into a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

// TestLoadConfig_Precedence is the reason this test file exists at M0: the
// flags > env > file > defaults ordering has to hold before anything reads
// configuration, because retrofitting it later silently changes the meaning of
// every existing deployment's settings.
func TestLoadConfig_Precedence(t *testing.T) {
	const fileBody = "log-level: debug\n"

	tests := map[string]struct {
		useFile bool
		env     map[string]string
		args    []string
		want    string
	}{
		"default wins when nothing is set": {
			want: "info",
		},
		"file beats default": {
			useFile: true,
			want:    "debug",
		},
		"env beats file": {
			useFile: true,
			env:     map[string]string{"KUBESPIN_LOG_LEVEL": "warn"},
			want:    "warn",
		},
		"env beats default": {
			env:  map[string]string{"KUBESPIN_LOG_LEVEL": "warn"},
			want: "warn",
		},
		"flag beats env": {
			env:  map[string]string{"KUBESPIN_LOG_LEVEL": "warn"},
			args: []string{"--log-level", "error"},
			want: "error",
		},
		"flag beats file": {
			useFile: true,
			args:    []string{"--log-level", "error"},
			want:    "error",
		},
		"flag beats env and file together": {
			useFile: true,
			env:     map[string]string{"KUBESPIN_LOG_LEVEL": "warn"},
			args:    []string{"--log-level", "error"},
			want:    "error",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			args := tc.args
			if tc.useFile {
				args = append([]string{"--config", writeConfig(t, fileBody)}, args...)
			}

			cfg, err := LoadConfig(newFlags(t, args...))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.LogLevel != tc.want {
				t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, tc.want)
			}
		})
	}
}

// A boolean flag left at its false default must still be overridable by env,
// which is the case viper's flag binding is easiest to get wrong on.
func TestLoadConfig_BoolPrecedence(t *testing.T) {
	t.Run("env sets dry-run", func(t *testing.T) {
		t.Setenv("KUBESPIN_DRY_RUN", "true")

		cfg, err := LoadConfig(newFlags(t))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if !cfg.DryRun {
			t.Error("DryRun = false, want true from KUBESPIN_DRY_RUN")
		}
	})

	t.Run("explicit flag beats env", func(t *testing.T) {
		t.Setenv("KUBESPIN_DRY_RUN", "true")

		cfg, err := LoadConfig(newFlags(t, "--dry-run=false"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.DryRun {
			t.Error("DryRun = true, want false from an explicit --dry-run=false")
		}
	})
}

func TestLoadConfig_NestedKeysFromFile(t *testing.T) {
	path := writeConfig(t, "registry-dsn: postgres://from-file\n")

	cfg, err := LoadConfig(newFlags(t, "--config", path))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Registry.DSN != "postgres://from-file" {
		t.Errorf("Registry.DSN = %q, want postgres://from-file", cfg.Registry.DSN)
	}
	if cfg.SourceFile != path {
		t.Errorf("SourceFile = %q, want %q", cfg.SourceFile, path)
	}
}

// The registry DSN carries a password, so it must only ever be readable from
// KUBESPIN_REGISTRY_DSN (or a config file) — never a CLI flag, which would
// leak it into shell history and process listings.
func TestLoadConfig_RegistryDSNFromEnv(t *testing.T) {
	t.Setenv("KUBESPIN_REGISTRY_DSN", "postgres://from-env")

	cfg, err := LoadConfig(newFlags(t))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Registry.DSN != "postgres://from-env" {
		t.Errorf("Registry.DSN = %q, want postgres://from-env", cfg.Registry.DSN)
	}
}

func TestLoadConfig_MissingExplicitFileIsAnError(t *testing.T) {
	// A missing default config file is fine; a missing file the user explicitly
	// pointed at is a typo worth failing on.
	_, err := LoadConfig(newFlags(t, "--config", filepath.Join(t.TempDir(), "nope.yaml")))
	if err == nil {
		t.Fatal("expected an error for a missing explicit config file")
	}
	if !errors.Is(err, ErrConfig) {
		t.Errorf("error %v does not wrap ErrConfig", err)
	}
}

func TestLoadConfig_RejectsInvalidValues(t *testing.T) {
	for name, args := range map[string][]string{
		"bad log level":  {"--log-level", "chatty"},
		"bad log format": {"--log-format", "xml"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfig(newFlags(t, args...)); !errors.Is(err, ErrConfig) {
				t.Errorf("error = %v, want one wrapping ErrConfig", err)
			}
		})
	}
}

func TestConfigLogger(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		cfg := &Config{LogLevel: "debug", LogFormat: format}
		if cfg.Logger(os.Stderr) == nil {
			t.Errorf("Logger() returned nil for format %q", format)
		}
	}
}
