package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/fleetinfra"
)

func TestBootstrap_RequiresAccountID(t *testing.T) {
	_, err := execute(t, "fleet", "bootstrap", "--region", "us-east-1")
	if err == nil {
		t.Fatal("expected an error when --account-id is missing")
	}
	if !strings.Contains(err.Error(), "account-id") {
		t.Errorf("error %q does not name the missing flag", err)
	}
}

func TestBootstrap_RequiresRegion(t *testing.T) {
	// Region has no default: bootstrapping into an unintended region would
	// create a second, silently orphaned ingestion API.
	_, err := execute(t, "fleet", "bootstrap", "--account-id", "123456789012")
	if err == nil {
		t.Fatal("expected an error when no region is configured")
	}
	if !strings.Contains(err.Error(), "region") {
		t.Errorf("error %q does not mention the missing region", err)
	}
}

// The handler is read from disk, so a missing build has to point at the fix
// rather than failing with a bare file-not-found.
func TestBootstrap_MissingHandlerExplainsHowToBuild(t *testing.T) {
	t.Setenv("KUBESPIN_REGISTRY_DSN", "postgres://user:pass@localhost:5432/kubespin?sslmode=disable")

	_, err := execute(t, "fleet", "bootstrap",
		"--account-id", "123456789012",
		"--region", "us-east-1",
		"--lambda-binary", filepath.Join(t.TempDir(), "absent"),
	)
	if err == nil {
		t.Fatal("expected an error for a missing handler binary")
	}
	if !strings.Contains(err.Error(), "make lambda") {
		t.Errorf("error %q does not tell the user how to build the handler", err)
	}
}

func TestBootstrap_PackagesHandlerFromDisk(t *testing.T) {
	// Stops before any AWS call: this only proves the spec is assembled and the
	// handler packaged from the given path.
	path := filepath.Join(t.TempDir(), "bootstrap")
	if err := os.WriteFile(path, []byte("compiled handler"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("writing fixture: %v", err)
	}

	cmd := newFleetBootstrapCommand()
	if err := cmd.Flags().Set("account-id", "123456789012"); err != nil {
		t.Fatalf("setting flag: %v", err)
	}
	if err := cmd.Flags().Set("region", "us-east-1"); err != nil {
		t.Fatalf("setting flag: %v", err)
	}
	if err := cmd.Flags().Set("lambda-binary", path); err != nil {
		t.Fatalf("setting flag: %v", err)
	}

	cfg := &Config{Registry: RegistryConfig{DSN: "postgres://user:pass@localhost:5432/kubespin?sslmode=disable"}}
	spec, err := bootstrapSpec(cmd, cfg)
	if err != nil {
		t.Fatalf("bootstrapSpec: %v", err)
	}

	if len(spec.LambdaZip) == 0 {
		t.Error("spec carries no packaged handler")
	}
	if spec.AccountID != "123456789012" || spec.Region != "us-east-1" {
		t.Errorf("spec = %+v, want the configured account and region", spec)
	}
	if err := spec.Validate(); err != nil {
		t.Errorf("assembled spec is invalid: %v", err)
	}
}

func TestPrintReport(t *testing.T) {
	tests := map[string]struct {
		report   fleetinfra.Report
		contains []string
	}{
		"dry run with changes": {
			report: fleetinfra.Report{
				DryRun:  true,
				Actions: []fleetinfra.Action{{Resource: "example resource", Kind: fleetinfra.ActionCreate}},
			},
			contains: []string{"example resource", "create", "re-run without --dry-run"},
		},
		"everything in sync": {
			report: fleetinfra.Report{
				Actions: []fleetinfra.Action{{Resource: "example resource", Kind: fleetinfra.ActionNone}},
			},
			contains: []string{"in sync", "already in sync"},
		},
		"applied changes report the endpoint": {
			report: fleetinfra.Report{
				Actions:      []fleetinfra.Action{{Resource: "ingestion API", Kind: fleetinfra.ActionCreate}},
				IngestionURL: "https://abc.execute-api.us-east-1.amazonaws.com/v1/clusters/{clusterId}/status",
			},
			contains: []string{"1 resource(s) changed", "egress allowlist", "execute-api"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cmd := newFleetBootstrapCommand()
			var out bytes.Buffer
			cmd.SetOut(&out)

			printReport(cmd, tc.report)

			for _, want := range tc.contains {
				if !strings.Contains(out.String(), want) {
					t.Errorf("report output is missing %q:\n%s", want, out.String())
				}
			}
		})
	}
}
