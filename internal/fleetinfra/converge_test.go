package fleetinfra

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// converge runs against the fake and fails the test on error.
func converge(t *testing.T, f *fakeAWS, dryRun bool) Report {
	t.Helper()

	report, err := Converge(context.Background(), f.clients(), testSpec(), dryRun)
	if err != nil {
		t.Fatalf("Converge: %v", err)
	}
	return report
}

// provisioned returns a fake that has already been fully converged, which is
// how every drift test starts.
func provisioned(t *testing.T) *fakeAWS {
	t.Helper()

	f := newFakeAWS()
	converge(t, f, false)
	f.calls = nil // forget the setup calls so assertions see only the run under test
	return f
}

func TestConverge_EmptyAccountCreatesEverything(t *testing.T) {
	f := newFakeAWS()
	report := converge(t, f, false)

	if len(report.Actions) != 6 {
		t.Fatalf("got %d actions, want 6", len(report.Actions))
	}
	for _, action := range report.Actions {
		if action.Kind != ActionCreate {
			t.Errorf("%s: kind = %s, want create", action.Resource, action.Kind)
		}
	}
	if report.Changed() != 6 {
		t.Errorf("Changed() = %d, want 6", report.Changed())
	}

	for _, want := range []string{"CreateTable", "CreateLogGroup", "CreateRole", "CreateFunction", "CreateApi", "AddPermission"} {
		if !f.called(want) {
			t.Errorf("%s was never called", want)
		}
	}
}

// TestConverge_SecondRunIsNoOp is the acceptance criterion for dropping
// Terraform: with no state file, convergence is only trustworthy if a run
// against already-provisioned infrastructure reports nothing to do.
func TestConverge_SecondRunIsNoOp(t *testing.T) {
	f := newFakeAWS()
	converge(t, f, false)

	f.calls = nil
	report := converge(t, f, false)

	for _, action := range report.Actions {
		if action.Kind != ActionNone {
			t.Errorf("%s: kind = %s (%s), want in sync",
				action.Resource, action.Kind, strings.Join(action.Details, "; "))
		}
	}
	if report.Changed() != 0 {
		t.Errorf("Changed() = %d, want 0", report.Changed())
	}
	f.assertNoMutations(t) // a no-op run must also be call-for-call inert
}

func TestConverge_DryRunMakesNoMutatingCalls(t *testing.T) {
	f := newFakeAWS()
	report := converge(t, f, true)

	f.assertNoMutations(t)

	if !report.DryRun {
		t.Error("report does not record that it was a dry run")
	}
	if report.Changed() != 6 {
		t.Errorf("Changed() = %d, want 6 — a dry run still reports what it would do", report.Changed())
	}
	if report.IngestionURL != "" {
		t.Errorf("IngestionURL = %q, want empty: the API does not exist yet", report.IngestionURL)
	}
}

func TestConverge_DryRunOnProvisionedAccountIsInSync(t *testing.T) {
	f := provisioned(t)
	report := converge(t, f, true)

	f.assertNoMutations(t)
	if report.Changed() != 0 {
		t.Errorf("Changed() = %d, want 0", report.Changed())
	}
}

func TestConverge_ReportsIngestionURL(t *testing.T) {
	f := newFakeAWS()
	report := converge(t, f, false)

	if !strings.HasSuffix(report.IngestionURL, statusRoutePath) {
		t.Errorf("IngestionURL = %q, want it to end with %q", report.IngestionURL, statusRoutePath)
	}
	if !strings.HasPrefix(report.IngestionURL, "https://") {
		t.Errorf("IngestionURL = %q, want an https endpoint", report.IngestionURL)
	}
}

// The guard that replaces Terraform's allowed_account_ids.
func TestConverge_RefusesWrongAccount(t *testing.T) {
	f := newFakeAWS()
	f.account = "999999999999"

	_, err := Converge(context.Background(), f.clients(), testSpec(), false)
	if !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("error = %v, want one wrapping ErrAccountMismatch", err)
	}
	if f.called("DescribeTable", "GetApis") {
		t.Error("converge inspected resources before the account check passed")
	}
	f.assertNoMutations(t)
}

func TestConverge_RejectsInvalidSpec(t *testing.T) {
	tests := map[string]func(*Spec){
		"short account id": func(s *Spec) { s.AccountID = "123" },
		"no region":        func(s *Spec) { s.Region = "" },
		"no table":         func(s *Spec) { s.RegistryTable = "" },
		"no lambda zip":    func(s *Spec) { s.LambdaZip = nil },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			spec := testSpec()
			mutate(&spec)

			f := newFakeAWS()
			if _, err := Converge(context.Background(), f.clients(), spec, false); !errors.Is(err, ErrSpec) {
				t.Fatalf("error = %v, want one wrapping ErrSpec", err)
			}
			if f.called("GetCallerIdentity") {
				t.Error("spec validation should fail before any AWS call")
			}
		})
	}
}

func TestConverge_DetectsDrift(t *testing.T) {
	tests := map[string]struct {
		drift    func(*fakeAWS)
		resource string
		wantCall string
	}{
		"log retention changed": {
			drift:    func(f *fakeAWS) { f.logGroups["/aws/lambda/kubespin-ingestion"] = aws.Int32(1) },
			resource: "log groups",
			wantCall: "PutRetentionPolicy",
		},
		"point-in-time recovery disabled": {
			drift:    func(f *fakeAWS) { f.pitrEnabled = false },
			resource: "registry table",
			wantCall: "UpdateContinuousBackups",
		},
		"deletion protection removed": {
			drift:    func(f *fakeAWS) { f.table.DeletionProtectionEnabled = aws.Bool(false) },
			resource: "registry table",
			wantCall: "UpdateTable",
		},
		"index dropped": {
			drift:    func(f *fakeAWS) { f.table.GlobalSecondaryIndexes = nil },
			resource: "registry table",
			wantCall: "UpdateTable",
		},
		"handler code changed": {
			drift:    func(f *fakeAWS) { f.function.CodeSha256 = aws.String("stale") },
			resource: "ingestion function",
			wantCall: "UpdateFunctionCode",
		},
		"throttle limits changed": {
			drift:    func(f *fakeAWS) { f.stage.DefaultRouteSettings.ThrottlingBurstLimit = aws.Int32(5) },
			resource: "ingestion API",
			wantCall: "UpdateStage",
		},
		"inline policy weakened": {
			drift: func(f *fakeAWS) {
				f.rolePolicy = "" // policy detached entirely
			},
			resource: "ingestion role",
			wantCall: "PutRolePolicy",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f := provisioned(t)
			tc.drift(f)

			// Dry run first: it must see the drift without repairing it.
			dry := converge(t, f, true)
			if !hasAction(dry, tc.resource, ActionUpdate) {
				t.Fatalf("dry run did not report an update for %s: %v", tc.resource, dry.Actions)
			}
			f.assertNoMutations(t)

			f.calls = nil
			if got := converge(t, f, false); !hasAction(got, tc.resource, ActionUpdate) {
				t.Fatalf("real run did not report an update for %s", tc.resource)
			}
			if !f.called(tc.wantCall) {
				t.Errorf("%s was never called; calls were %v", tc.wantCall, f.calls)
			}

			// And the repair must converge: a third run is a no-op again.
			f.calls = nil
			after := converge(t, f, false)
			if after.Changed() != 0 {
				t.Errorf("run after repair still reports %d changes: %v", after.Changed(), after.Actions)
			}
		})
	}
}

// Three resources share the AWS name "<prefix>-ingestion" (role, function,
// API), so report lines have to be named after their step. Without this, both
// the printed report and the drift assertions above become ambiguous.
func TestConverge_ReportsDistinctResourceNames(t *testing.T) {
	report := converge(t, newFakeAWS(), true)

	seen := map[string]bool{}
	for _, action := range report.Actions {
		if seen[action.Resource] {
			t.Errorf("two report lines both named %q", action.Resource)
		}
		seen[action.Resource] = true
	}
}

func TestConverge_RecreatesMissingTable(t *testing.T) {
	f := provisioned(t)
	f.table = nil
	f.pitrEnabled = false

	report := converge(t, f, false)
	if !hasAction(report, "registry table", ActionCreate) {
		t.Fatalf("missing table was not planned for creation: %v", report.Actions)
	}
	if !f.called("CreateTable") {
		t.Error("CreateTable was never called")
	}
}

func TestConverge_StopsAtFirstFailure(t *testing.T) {
	f := &failingDynamo{fakeAWS: newFakeAWS()}

	_, err := Converge(context.Background(), f.clients(), testSpec(), false)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "registry table") {
		t.Errorf("error %q does not name the failing step", err)
	}
	// Later steps must not have run.
	if f.called("CreateApi", "CreateFunction", "CreateRole") {
		t.Error("converge continued past a failed step")
	}
}

// failingDynamo makes table creation fail while leaving every other service
// working, to prove the run stops rather than pressing on.
type failingDynamo struct{ *fakeAWS }

func (f *failingDynamo) CreateTable(_ context.Context, _ *dynamodb.CreateTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	f.record("CreateTable")
	return nil, errors.New("insufficient permissions")
}

func (f *failingDynamo) clients() *Clients {
	c := f.fakeAWS.clients()
	c.dynamo = f
	return c
}

func hasAction(r Report, resource string, kind ActionKind) bool {
	for _, action := range r.Actions {
		if action.Resource == resource && action.Kind == kind {
			return true
		}
	}
	return false
}
