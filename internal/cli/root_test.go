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

	for _, want := range []string{"apply", "delete", "login", "status", "logout"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output is missing the %q command:\n%s", want, out)
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

// Neither apply nor delete should exit zero without a registry to talk to.
// Reaching that specific error (rather than, say, a flag-parsing failure)
// proves PersistentPreRunE resolved config and the command's own body
// actually ran.
func TestCommandsRequireRegistryDSN(t *testing.T) {
	for _, args := range [][]string{
		{"delete", "--cluster-id", "team-payments-prod", "--provider", "aws", "--region", "us-east-1",
			"--subnets", "subnet-a", "--yes"},
		{"apply", "--cluster-id", "team-payments-prod", "--provider", "aws", "--region", "us-east-1",
			"--subnets", "subnet-a", "--github-org", "GitOpsHub"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := execute(t, args...)
			if !errors.Is(err, ErrConfig) {
				t.Errorf("error = %v, want one wrapping ErrConfig", err)
			}
			if !strings.Contains(err.Error(), "KUBESPIN_REGISTRY_DSN") {
				t.Errorf("error = %v, want it to name KUBESPIN_REGISTRY_DSN", err)
			}
		})
	}
}

func TestPersistentPreRunPopulatesContext(t *testing.T) {
	// delete's own body fails with a missing-DSN error, but only after
	// PersistentPreRunE has run — so reaching that error (rather than a panic
	// from a nil context value) proves config resolution succeeded.
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"delete", "--cluster-id", "team-payments-prod", "--provider", "aws",
		"--region", "us-east-1", "--subnets", "subnet-a", "--yes", "--log-level", "debug"})

	err := root.Execute()
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "KUBESPIN_REGISTRY_DSN") {
		t.Fatalf("error = %v, want one wrapping ErrConfig naming KUBESPIN_REGISTRY_DSN", err)
	}
}

func TestInvalidGlobalFlagFailsBeforeCommandRuns(t *testing.T) {
	_, err := execute(t, "delete", "--cluster-id", "team-payments-prod", "--provider", "aws",
		"--region", "us-east-1", "--subnets", "subnet-a", "--yes", "--log-level", "chatty")
	if !errors.Is(err, ErrConfig) {
		t.Errorf("error = %v, want one wrapping ErrConfig", err)
	}
	if strings.Contains(err.Error(), "KUBESPIN_REGISTRY_DSN") {
		t.Error("command body ran despite invalid configuration: got the missing-DSN error instead of the log-level one")
	}
}
