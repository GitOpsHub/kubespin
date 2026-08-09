package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
	"github.com/GitOpsHub/kubespin/internal/registry"
)

// fakeCloud records the provisioner calls a run makes, in order.
type fakeCloud struct {
	calls []string

	statuses  []provisioner.Status
	describes int

	createErr   error
	identityErr error
	egressErr   error
}

func newFakeCloud() *fakeCloud {
	return &fakeCloud{statuses: []provisioner.Status{provisioner.StatusActive}}
}

func (f *fakeCloud) Provider() core.Provider { return core.ProviderAWS }

func (f *fakeCloud) Create(context.Context, core.ClusterSpec) error {
	f.calls = append(f.calls, "Create")
	return f.createErr
}

func (f *fakeCloud) Describe(context.Context, core.ClusterSpec) (provisioner.ClusterState, error) {
	f.calls = append(f.calls, "Describe")

	status := f.statuses[min(f.describes, len(f.statuses)-1)]
	f.describes++
	return provisioner.ClusterState{Status: status, NetworkID: "sg-cluster", OIDCIssuer: "https://issuer"}, nil
}

func (f *fakeCloud) Reconcile(context.Context, core.ClusterSpec) (provisioner.Change, error) {
	f.calls = append(f.calls, "Reconcile")
	return provisioner.Change{}, nil
}

func (f *fakeCloud) Delete(context.Context, core.ClusterSpec) error {
	f.calls = append(f.calls, "Delete")
	return nil
}

func (f *fakeCloud) ProvisionForComponent(
	context.Context, core.ClusterSpec, provisioner.Component,
) (provisioner.Binding, error) {
	f.calls = append(f.calls, "ProvisionForComponent")
	if f.identityErr != nil {
		return provisioner.Binding{}, f.identityErr
	}
	return provisioner.Binding{Identifier: "arn:aws:iam::123456789012:role/reporter"}, nil
}

func (f *fakeCloud) Deprovision(context.Context, core.ClusterSpec, provisioner.Component) error {
	f.calls = append(f.calls, "Deprovision")
	return nil
}

func (f *fakeCloud) AllowEgress(
	context.Context, core.ClusterSpec, provisioner.EgressDestination,
) (provisioner.Change, error) {
	f.calls = append(f.calls, "AllowEgress")
	return provisioner.Change{Changed: true}, f.egressErr
}

func (f *fakeCloud) cloud() Cloud {
	return Cloud{
		Cluster:  f,
		Identity: f,
		Network:  f,
		IngestionEndpoint: provisioner.EgressDestination{
			Host: "abc.execute-api.us-east-1.amazonaws.com", Port: 443,
		},
		Wait: provisioner.WaitOptions{Interval: time.Millisecond, Timeout: time.Second},
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func runWithCloud(t *testing.T, f *fakeCloud) (registry.Record, error) {
	t.Helper()

	reg := registry.NewMemory()
	o := New(reg,
		WithSteps(ProvisioningSteps(f.cloud(), quietLogger())),
		WithHolder("test-runner"),
		WithLogger(quietLogger()),
	)
	return o.Apply(t.Context(), testSpec())
}

func TestProvisioningSteps_DriveTheProvisioners(t *testing.T) {
	f := newFakeCloud()

	rec, err := runWithCloud(t, f)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rec.Phase != core.PhaseReady {
		t.Errorf("Phase = %s, want ready", rec.Phase)
	}

	for _, want := range []string{"Create", "Describe", "Reconcile", "AllowEgress", "ProvisionForComponent"} {
		if !slices.Contains(f.calls, want) {
			t.Errorf("%s was never called; calls were %v", want, f.calls)
		}
	}

	// Identity binding needs the issuer, which only exists once the control
	// plane is up — so it must follow the wait, not race it.
	create := slices.Index(f.calls, "Create")
	identity := slices.Index(f.calls, "ProvisionForComponent")
	if create > identity {
		t.Errorf("calls were %v, want the cluster created before identity is bound", f.calls)
	}
}

// The phase must not advance while the control plane is still coming up, or a
// resumed run would believe a half-built cluster was finished.
func TestProvisioningSteps_WaitForTheControlPlane(t *testing.T) {
	f := newFakeCloud()
	f.statuses = []provisioner.Status{
		provisioner.StatusAbsent,
		provisioner.StatusCreating,
		provisioner.StatusActive,
	}

	if _, err := runWithCloud(t, f); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if f.describes < 3 {
		t.Errorf("Describe called %d times, want it to poll until active", f.describes)
	}
}

func TestProvisioningSteps_FailedClusterStopsTheRun(t *testing.T) {
	f := newFakeCloud()
	f.statuses = []provisioner.Status{provisioner.StatusFailed}

	rec, err := runWithCloud(t, f)
	if !errors.Is(err, provisioner.ErrClusterFailed) {
		t.Fatalf("error = %v, want one wrapping ErrClusterFailed", err)
	}
	// The phase stays where it was, so a retry re-runs cluster creation.
	if rec.Phase != core.PhasePending {
		t.Errorf("Phase = %s, want pending after a failed creation", rec.Phase)
	}
	if slices.Contains(f.calls, "ProvisionForComponent") {
		t.Error("identity was bound despite the cluster failing")
	}
}

func TestProvisioningSteps_EgressFailureStopsTheRun(t *testing.T) {
	// A cluster that cannot reach the ingestion API is invisible to the fleet,
	// so this is a failure rather than a warning.
	f := newFakeCloud()
	f.egressErr = errors.New("insufficient permissions")

	if _, err := runWithCloud(t, f); err == nil {
		t.Fatal("expected the egress failure to surface")
	}
}

// Without a configured endpoint there is nothing to allow, but the run should
// say so rather than silently produce a cluster that cannot report.
func TestProvisioningSteps_MissingIngestionEndpointIsNotFatal(t *testing.T) {
	f := newFakeCloud()
	cloud := f.cloud()
	cloud.IngestionEndpoint = provisioner.EgressDestination{}

	reg := registry.NewMemory()
	o := New(reg, WithSteps(ProvisioningSteps(cloud, quietLogger())), WithLogger(quietLogger()))

	rec, err := o.Apply(t.Context(), testSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rec.Phase != core.PhaseReady {
		t.Errorf("Phase = %s, want ready", rec.Phase)
	}
	if slices.Contains(f.calls, "AllowEgress") {
		t.Error("egress was opened without a configured destination")
	}
}

// Repository seeding and the Argo CD bootstrap are still no-ops, so a run
// reaches ready with a real cluster and identity but no addons — exactly the M2
// gate and no more.
func TestProvisioningSteps_LaterPhasesRemainNoOps(t *testing.T) {
	steps := ProvisioningSteps(newFakeCloud().cloud(), quietLogger())

	for _, phase := range []core.Phase{core.PhaseIdentityBound, core.PhaseRepoPushed, core.PhaseArgoCDInstalled} {
		step, ok := steps[phase]
		if !ok {
			t.Fatalf("no step registered for %s", phase)
		}
		if err := step.Run(t.Context(), testSpec(), registry.Record{}); err != nil {
			t.Errorf("step for %s returned %v, want a no-op", phase, err)
		}
	}
}
