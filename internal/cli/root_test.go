package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// execute runs the root command with args, capturing its output.
func execute(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)

	err := root.Execute()
	return out.String(), err
}

func TestRootHelpListsEveryCommand(t *testing.T) {
	out, err := execute(t, "--help")
	if err != nil {
		t.Fatalf("--help returned %v", err)
	}

	for _, want := range []string{"apply", "delete", "fleet"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output is missing the %q command:\n%s", want, out)
		}
	}
}

func TestFleetHelpListsSubcommands(t *testing.T) {
	out, err := execute(t, "fleet", "--help")
	if err != nil {
		t.Fatalf("fleet --help returned %v", err)
	}

	for _, want := range []string{"update", "audit", "status"} {
		if !strings.Contains(out, want) {
			t.Errorf("fleet help is missing the %q subcommand:\n%s", want, out)
		}
	}
}

func TestVersionFlag(t *testing.T) {
	out, err := execute(t, "--version")
	if err != nil {
		t.Fatalf("--version returned %v", err)
	}
	if !strings.Contains(out, "kubespin") {
		t.Errorf("version output = %q", out)
	}
}

// Every command is implemented as of M9: none of them should exit zero
// without a Fleet Registry to talk to. Reaching that specific error (rather
// than, say, a flag-parsing failure) proves PersistentPreRunE resolved
// config and the command's own body actually ran.
func TestCommandsRequireRegistryRegion(t *testing.T) {
	for _, args := range [][]string{
		{"delete", "--cluster-id", "team-payments-prod", "--provider", "aws", "--region", "us-east-1",
			"--profile", "tier-small@1.0.0", "--subnets", "subnet-a", "--yes"},
		{"fleet", "update", "--component", "cert-manager", "--version", "1.16.0"},
		{"fleet", "audit"},
		{"fleet", "status"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := execute(t, args...)
			if !errors.Is(err, ErrConfig) {
				t.Errorf("error = %v, want one wrapping ErrConfig", err)
			}
			if !strings.Contains(err.Error(), "registry-region") {
				t.Errorf("error = %v, want it to name --registry-region", err)
			}
		})
	}
}

func TestFleetWithNoSubcommandPrintsHelp(t *testing.T) {
	out, err := execute(t, "fleet")
	if err != nil {
		t.Fatalf("bare fleet returned %v", err)
	}
	if !strings.Contains(out, "audit") {
		t.Errorf("bare fleet did not print help:\n%s", out)
	}
}

func TestPersistentPreRunPopulatesContext(t *testing.T) {
	// fleet status's own body fails with a registry-region error, but only
	// after PersistentPreRunE has run — so reaching that error (rather than a
	// panic from a nil context value) proves config resolution succeeded.
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fleet", "status", "--log-level", "debug"})

	err := root.Execute()
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "registry-region") {
		t.Fatalf("error = %v, want one wrapping ErrConfig naming --registry-region", err)
	}
}

func TestInvalidGlobalFlagFailsBeforeCommandRuns(t *testing.T) {
	_, err := execute(t, "fleet", "status", "--log-level", "chatty")
	if !errors.Is(err, ErrConfig) {
		t.Errorf("error = %v, want one wrapping ErrConfig", err)
	}
	if strings.Contains(err.Error(), "registry-region") {
		t.Error("command body ran despite invalid configuration: got the registry-region error instead of the log-level one")
	}
}
