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

// Scaffolded commands must fail rather than exit zero and imply they did work.
func TestStubCommandsReportNotImplemented(t *testing.T) {
	// apply is implemented from M2 onward; the rest are still scaffolded.
	for _, args := range [][]string{
		{"delete"},
		{"fleet", "update"},
		{"fleet", "audit"},
		{"fleet", "status"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := execute(t, args...); !errors.Is(err, ErrNotImplemented) {
				t.Errorf("error = %v, want one wrapping ErrNotImplemented", err)
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
	// The stub returns ErrNotImplemented, but only after PersistentPreRunE has
	// run — so reaching that error proves config resolution succeeded.
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"delete", "--log-level", "debug"})

	if err := root.Execute(); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("error = %v, want one wrapping ErrNotImplemented", err)
	}
}

func TestInvalidGlobalFlagFailsBeforeCommandRuns(t *testing.T) {
	_, err := execute(t, "delete", "--log-level", "chatty")
	if !errors.Is(err, ErrConfig) {
		t.Errorf("error = %v, want one wrapping ErrConfig", err)
	}
	if errors.Is(err, ErrNotImplemented) {
		t.Error("command body ran despite invalid configuration")
	}
}
