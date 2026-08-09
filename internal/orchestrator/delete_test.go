package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/registry"
	"github.com/GitOpsHub/kubespin/internal/repo"
)

// seedReadyCluster registers and advances spec to PhaseReady, the state a
// cluster is in before anyone would call Delete on it.
func seedReadyCluster(t *testing.T, reg registry.Registry, spec core.ClusterSpec) {
	t.Helper()
	if _, err := newOrchestrator(t, reg, newRecorder().steps()).Apply(t.Context(), spec); err != nil {
		t.Fatalf("seeding a ready cluster: %v", err)
	}
}

func TestDelete_TearsDownAReadyCluster(t *testing.T) {
	reg := registry.NewMemory()
	spec := testSpec()
	seedReadyCluster(t, reg, spec)

	var torndown bool
	o := newOrchestrator(t, reg, newRecorder().steps())

	rec, err := o.Delete(t.Context(), spec, func(context.Context, core.ClusterSpec, registry.Record) error {
		torndown = true
		return nil
	})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if rec.Phase != core.PhaseDecommissioned {
		t.Errorf("Phase = %s, want decommissioned", rec.Phase)
	}
	if !torndown {
		t.Error("teardown was never called")
	}

	stored, err := reg.Get(t.Context(), spec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Phase != core.PhaseDecommissioned {
		t.Errorf("stored phase = %s, want decommissioned", stored.Phase)
	}
}

func TestDelete_IsIdempotent(t *testing.T) {
	reg := registry.NewMemory()
	spec := testSpec()
	seedReadyCluster(t, reg, spec)

	o := newOrchestrator(t, reg, newRecorder().steps())
	noop := func(context.Context, core.ClusterSpec, registry.Record) error { return nil }

	if _, err := o.Delete(t.Context(), spec, noop); err != nil {
		t.Fatalf("first Delete: %v", err)
	}

	var calledAgain bool
	rec, err := o.Delete(t.Context(), spec, func(context.Context, core.ClusterSpec, registry.Record) error {
		calledAgain = true
		return nil
	})
	if err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if calledAgain {
		t.Error("teardown ran again on an already-decommissioned cluster")
	}
	if rec.Phase != core.PhaseDecommissioned {
		t.Errorf("Phase = %s, want decommissioned", rec.Phase)
	}
}

// A crashed teardown must leave the cluster resumable: retrying Delete has to
// run teardown again rather than believing decommissioning already finished.
func TestDelete_ResumesAfterAFailedTeardown(t *testing.T) {
	reg := registry.NewMemory()
	spec := testSpec()
	seedReadyCluster(t, reg, spec)

	o := newOrchestrator(t, reg, newRecorder().steps())
	sentinel := errors.New("cloud API unavailable")

	if _, err := o.Delete(t.Context(), spec, func(context.Context, core.ClusterSpec, registry.Record) error {
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap the teardown failure", err)
	}

	stored, err := reg.Get(t.Context(), spec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Phase != core.PhaseDecommissioning {
		t.Errorf("phase after a failed teardown = %s, want decommissioning so a retry resumes it", stored.Phase)
	}

	var retried bool
	rec, err := o.Delete(t.Context(), spec, func(context.Context, core.ClusterSpec, registry.Record) error {
		retried = true
		return nil
	})
	if err != nil {
		t.Fatalf("retried Delete: %v", err)
	}
	if !retried {
		t.Error("retried Delete did not run teardown again")
	}
	if rec.Phase != core.PhaseDecommissioned {
		t.Errorf("Phase = %s, want decommissioned", rec.Phase)
	}
}

func TestDelete_UnknownClusterErrors(t *testing.T) {
	reg := registry.NewMemory()
	o := newOrchestrator(t, reg, newRecorder().steps())

	_, err := o.Delete(t.Context(), testSpec(), func(context.Context, core.ClusterSpec, registry.Record) error {
		return nil
	})
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("error = %v, want one wrapping ErrNotFound", err)
	}
}

// Teardown builds a real TeardownFunc; this proves the ordering and that
// every sub-step gets called with real (fake) provisioners.
func TestTeardown_CallsIdentityThenClusterThenRepo(t *testing.T) {
	f := newFakeCloud()
	repoProv := repo.NewMemory()
	spec := testSpec()

	if err := repoProv.Create(t.Context(), spec); err != nil {
		t.Fatalf("seeding repo: %v", err)
	}

	teardown := Teardown(f.cloud(), repoProv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := teardown(t.Context(), spec, registry.Record{}); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	deprovision := slices.Index(f.calls, "Deprovision")
	del := slices.Index(f.calls, "Delete")
	if deprovision < 0 || del < 0 || deprovision > del {
		t.Errorf("calls = %v, want Deprovision before Delete", f.calls)
	}
	if !repoProv.Archived(spec) {
		t.Error("expected the repository to have been archived")
	}
}

// Delete only requests the teardown; the cluster lingers in a deleting state
// for minutes afterwards. Teardown has to see it gone before it returns, or
// the caller records decommissioned while the cloud is still working.
func TestTeardown_WaitsForTheClusterToBeGone(t *testing.T) {
	f := newFakeCloud()
	f.deletingPolls = 3
	spec := testSpec()

	repoProv := repo.NewMemory()
	if err := repoProv.Create(t.Context(), spec); err != nil {
		t.Fatalf("seeding repo: %v", err)
	}

	teardown := Teardown(f.cloud(), repoProv, quietLogger())
	if err := teardown(t.Context(), spec, registry.Record{}); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	del := slices.Index(f.calls, "Delete")
	polls := 0
	for _, call := range f.calls[del:] {
		if call == "Describe" {
			polls++
		}
	}
	if polls < 4 { // three deleting answers, then absent
		t.Errorf("calls after Delete = %v, want Describe polled until the cluster was absent", f.calls[del:])
	}
}

// A cluster the cloud never finishes deleting must fail the teardown, leaving
// the phase at decommissioning for a retry — not archive the repo and let the
// caller mark it decommissioned.
func TestTeardown_FailsWhenTheClusterNeverGoesAway(t *testing.T) {
	f := newFakeCloud()
	f.deletingPolls = 1_000_000
	spec := testSpec()

	repoProv := repo.NewMemory()
	if err := repoProv.Create(t.Context(), spec); err != nil {
		t.Fatalf("seeding repo: %v", err)
	}

	err := Teardown(f.cloud(), repoProv, quietLogger())(t.Context(), spec, registry.Record{})
	if err == nil {
		t.Fatal("Teardown succeeded, want a timeout waiting for the cluster to be deleted")
	}
	if repoProv.Archived(spec) {
		t.Error("the repository was archived even though the cluster was never deleted")
	}
}
