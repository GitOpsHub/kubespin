package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// applyCmd returns the apply command with args parsed, ready for loadSpec.
func applyCmd(t *testing.T, args ...string) *cobra.Command {
	t.Helper()

	cmd := newApplyCommand()
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}
	return cmd
}

func writeSpec(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing spec: %v", err)
	}
	return path
}

const validSpecYAML = `
id: team-payments-prod
provider: aws
region: us-east-1
access: private
kubernetesVersion: "1.34"
subnets:
  - subnet-aaa
  - subnet-bbb
profile:
  name: tier-small
  version: 1.0.0
nodePools:
  - name: default
    instanceType: m6i.large
    minSize: 1
    maxSize: 5
    desiredSize: 3
  - name: spot
    instanceType: m6i.xlarge
    minSize: 0
    maxSize: 10
    desiredSize: 2
`

func TestLoadSpec_FromFile(t *testing.T) {
	spec, err := loadSpec(applyCmd(t, "--spec", writeSpec(t, validSpecYAML)))
	if err != nil {
		t.Fatalf("loadSpec: %v", err)
	}

	if spec.ID != "team-payments-prod" || spec.Provider != core.ProviderAWS {
		t.Errorf("spec = %+v, want the file's values", spec)
	}
	if len(spec.NodePools) != 2 {
		t.Errorf("NodePools = %d, want both from the file", len(spec.NodePools))
	}
	if len(spec.Subnets) != 2 {
		t.Errorf("Subnets = %v, want both from the file", spec.Subnets)
	}
	if spec.Profile.Name != "tier-small" || spec.Profile.Version != "1.0.0" {
		t.Errorf("Profile = %+v, want tier-small@1.0.0", spec.Profile)
	}
}

func TestLoadSpec_FromFlags(t *testing.T) {
	spec, err := loadSpec(applyCmd(t,
		"--cluster-id", "team-alpha",
		"--provider", "aws",
		"--region", "eu-west-1",
		"--profile", "tier-small@1.0.0",
		"--subnets", "subnet-aaa,subnet-bbb",
	))
	if err != nil {
		t.Fatalf("loadSpec: %v", err)
	}

	if spec.ID != "team-alpha" || spec.Region != "eu-west-1" {
		t.Errorf("spec = %+v, want the flag values", spec)
	}
	// Private is the default: a cluster should not become publicly reachable
	// because someone forgot a flag.
	if spec.Access != core.AccessPrivate {
		t.Errorf("Access = %s, want private by default", spec.Access)
	}
	if len(spec.NodePools) != 1 || spec.NodePools[0].Name != defaultPoolName {
		t.Errorf("NodePools = %+v, want one default pool", spec.NodePools)
	}
}

// A file supplies the base; an explicitly-set flag overrides it. A flag left at
// its default must not quietly replace what the file said.
func TestLoadSpec_FlagsOverrideFileOnlyWhenSet(t *testing.T) {
	path := writeSpec(t, validSpecYAML)

	t.Run("explicit flag wins", func(t *testing.T) {
		spec, err := loadSpec(applyCmd(t, "--spec", path, "--region", "eu-west-1"))
		if err != nil {
			t.Fatalf("loadSpec: %v", err)
		}
		if spec.Region != "eu-west-1" {
			t.Errorf("Region = %q, want the flag to override the file", spec.Region)
		}
	})

	t.Run("unset flag leaves the file alone", func(t *testing.T) {
		// --access defaults to private and the file also says private; the
		// meaningful case is a file value the default would clobber.
		spec, err := loadSpec(applyCmd(t, "--spec", path))
		if err != nil {
			t.Fatalf("loadSpec: %v", err)
		}
		if spec.KubernetesVersion != "1.34" {
			t.Errorf("KubernetesVersion = %q, want the file's value", spec.KubernetesVersion)
		}
		if len(spec.NodePools) != 2 {
			t.Errorf("NodePools = %d, want the file's pools rather than a synthesised default",
				len(spec.NodePools))
		}
	})
}

// A mistyped key that silently does nothing is worse than a failure.
func TestLoadSpec_RejectsUnknownFields(t *testing.T) {
	path := writeSpec(t, validSpecYAML+"\nnodepoolz: oops\n")

	_, err := loadSpec(applyCmd(t, "--spec", path))
	if err == nil {
		t.Fatal("expected an error for an unknown field")
	}
	if !strings.Contains(err.Error(), "nodepoolz") {
		t.Errorf("error %q does not name the offending field", err)
	}
}

func TestLoadSpec_Invalid(t *testing.T) {
	tests := map[string]struct {
		args    []string
		wantMsg string
	}{
		"no cluster id": {
			[]string{"--provider", "aws", "--region", "us-east-1", "--subnets", "a,b", "--profile", "tier-small@1.0.0"},
			"cluster id",
		},
		"no subnets": {
			[]string{"--cluster-id", "team-alpha", "--provider", "aws", "--region", "us-east-1", "--profile", "tier-small@1.0.0"},
			"subnet",
		},
		"bad profile reference": {
			[]string{"--cluster-id", "team-alpha", "--provider", "aws", "--region", "us-east-1", "--subnets", "a,b", "--profile", "tier-small"},
			"name@version",
		},
		"missing file": {
			[]string{"--spec", "/nonexistent/cluster.yaml"},
			"reading spec file",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := loadSpec(applyCmd(t, tc.args...))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tc.wantMsg)
			}
		})
	}
}

func TestParseProfileRef(t *testing.T) {
	ref, err := parseProfileRef("tier-regulated@2.1.0")
	if err != nil {
		t.Fatalf("parseProfileRef: %v", err)
	}
	if ref.Name != "tier-regulated" || ref.Version != "2.1.0" {
		t.Errorf("ref = %+v, want tier-regulated@2.1.0", ref)
	}

	if _, err := parseProfileRef("tier-small"); !errors.Is(err, core.ErrInvalidSpec) {
		t.Errorf("error = %v, want one wrapping ErrInvalidSpec", err)
	}
}

func TestApply_UnimplementedProviders(t *testing.T) {
	// GCP and Azure must fail with a clear statement rather than a generic
	// error, since the interfaces exist and only the implementations do not.
	for _, provider := range []string{"gcp", "azure"} {
		t.Run(provider, func(t *testing.T) {
			cmd := applyCmd(t, "--ingestion-endpoint", "example.com")
			spec := core.ClusterSpec{Provider: core.Provider(provider), Region: "r"}

			_, err := buildCloud(t.Context(), cmd, spec)
			if !errors.Is(err, ErrProviderNotImplemented) {
				t.Fatalf("error = %v, want one wrapping ErrProviderNotImplemented", err)
			}
		})
	}
}
