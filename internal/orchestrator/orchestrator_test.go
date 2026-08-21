package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/registry"
)

func testSpec() core.ClusterSpec {
	return core.ClusterSpec{
		ID:       "team-payments-prod",
		Provider: core.ProviderAWS,
		Region:   "us-east-1",
		Access:   core.AccessPrivate,
		NodePools: []core.NodePool{{
			Name: "default", InstanceType: "m6i.large", MinSize: 1, MaxSize: 3, DesiredSize: 2,
		}},
		Size:    core.SizeSmall,
		Subnets: []string{"subnet-aaa", "subnet-bbb"},
	}
}

// recorder captures which steps ran, in order.
type recorder struct {
	mu    sync.Mutex
	calls []string
	fail  map[core.Phase]error
}

func newRecorder() *recorder { return &recorder{fail: map[core.Phase]error{}} }

// steps builds a step set that records each invocation against the recorder.
func (r *recorder) steps() map[core.Phase]Step {
	out := map[core.Phase]Step{}
	for phase, step := range DefaultSteps() {
		label := step.Name()
		out[phase] = StepFunc{
			Label: label,
			Fn: func(context.Context, core.ClusterSpec, registry.Record) error {
				r.mu.Lock()
				defer r.mu.Unlock()

				r.calls = append(r.calls, label)
				return r.fail[phase]
			},
		}
	}
	return out
}

func (r *recorder) ran() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func newOrchestrator(t *testing.T, reg registry.Registry, steps map[core.Phase]Step) *Orchestrator {
	t.Helper()

	return New(reg,
		WithSteps(steps),
		WithHolder("test-runner"),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
}

func TestApply_ProvisionsAFreshCluster(t *testing.T) {
	reg := registry.NewMemory()
	rec := newRecorder()
	o := newOrchestrator(t, reg, rec.steps())

	got, err := o.Apply(t.Context(), testSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got.Phase != core.PhaseReady {
		t.Errorf("Phase = %s, want ready", got.Phase)
	}

	want := []string{
		"create cluster",
		"bind workload identity",
		"create and seed repository",
		"install Argo CD",
		"verify addons healthy",
	}
	if diff := stepDiff(rec.ran(), want); diff != "" {
		t.Errorf("steps ran in the wrong order: %s", diff)
	}
}

// A second apply over a ready cluster must do nothing at all.
func TestApply_IsIdempotent(t *testing.T) {
	reg := registry.NewMemory()
	spec := testSpec()

	first := newRecorder()
	if _, err := newOrchestrator(t, reg, first.steps()).Apply(t.Context(), spec); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	before, err := reg.Get(t.Context(), spec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	second := newRecorder()
	got, err := newOrchestrator(t, reg, second.steps()).Apply(t.Context(), spec)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}

	if len(second.ran()) != 0 {
		t.Errorf("second apply ran %v, want no steps", second.ran())
	}
	if got.Version != before.Version {
		t.Errorf("Version = %d, want it unchanged at %d — a no-op apply must not write",
			got.Version, before.Version)
	}
}

// The property the orchestrator exists for: a failed run resumes where it
// stopped, and re-runs only the step that failed.
func TestApply_ResumesFromTheRecordedPhase(t *testing.T) {
	reg := registry.NewMemory()
	spec := testSpec()

	failing := newRecorder()
	failing.fail[core.PhaseIdentityBound] = errors.New("github is down")

	if _, err := newOrchestrator(t, reg, failing.steps()).Apply(t.Context(), spec); err == nil {
		t.Fatal("expected the seeded failure to surface")
	}

	stored, err := reg.Get(t.Context(), spec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Phase != core.PhaseIdentityBound {
		t.Fatalf("Phase = %s, want identity-bound: the phase must not advance past a failed step", stored.Phase)
	}

	// The retry re-runs the failed step and everything after it, and nothing
	// before it.
	retry := newRecorder()
	got, err := newOrchestrator(t, reg, retry.steps()).Apply(t.Context(), spec)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got.Phase != core.PhaseReady {
		t.Errorf("Phase = %s, want ready", got.Phase)
	}

	want := []string{"create and seed repository", "install Argo CD", "verify addons healthy"}
	if diff := stepDiff(retry.ran(), want); diff != "" {
		t.Errorf("retry did not resume correctly: %s", diff)
	}
}

func TestApply_StopsAtTheFailingStep(t *testing.T) {
	reg := registry.NewMemory()
	rec := newRecorder()
	sentinel := errors.New("subnet quota exceeded")
	rec.fail[core.PhasePending] = sentinel

	_, err := newOrchestrator(t, reg, rec.steps()).Apply(t.Context(), testSpec())
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap the step's error", err)
	}
	if !strings.Contains(err.Error(), "create cluster") {
		t.Errorf("error %q does not name the failing step", err)
	}

	if ran := rec.ran(); len(ran) != 1 {
		t.Errorf("steps ran = %v, want only the first", ran)
	}
}

// The lease must be released whether the run succeeds or fails, so the next
// run is not blocked until the TTL elapses.
func TestApply_ReleasesTheLease(t *testing.T) {
	for name, failAt := range map[string]core.Phase{
		"on success": "",
		"on failure": core.PhasePending,
	} {
		t.Run(name, func(t *testing.T) {
			reg := registry.NewMemory()
			rec := newRecorder()
			if failAt != "" {
				rec.fail[failAt] = errors.New("boom")
			}

			_, _ = newOrchestrator(t, reg, rec.steps()).Apply(t.Context(), testSpec())

			stored, err := reg.Get(t.Context(), testSpec().ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if stored.Held(time.Now()) {
				t.Errorf("lease still held by %s after the run finished", stored.Lease.Holder)
			}
		})
	}
}

func TestApply_RefusesWhenAnotherRunHoldsTheLease(t *testing.T) {
	reg := registry.NewMemory()
	spec := testSpec()

	// Register the cluster, then let a different run take the lease.
	if _, err := reg.Create(t.Context(), registry.NewRecord(spec, time.Now())); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := reg.AcquireLease(t.Context(), spec.ID, "other-runner", time.Hour); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	rec := newRecorder()
	_, err := newOrchestrator(t, reg, rec.steps()).Apply(t.Context(), spec)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("error = %v, want one wrapping ErrBusy", err)
	}
	if ran := rec.ran(); len(ran) != 0 {
		t.Errorf("steps ran = %v, want none while another run holds the lease", ran)
	}
}

// Two applies racing on the same cluster: one provisions, the other is refused.
func TestApply_ConcurrentRunsElectOneProvisioner(t *testing.T) {
	reg := registry.NewMemory()
	spec := testSpec()

	var (
		start sync.WaitGroup
		done  sync.WaitGroup
		mu    sync.Mutex
		errs  []error
	)
	start.Add(1)

	for i := range 2 {
		done.Add(1)
		go func() {
			defer done.Done()

			o := New(reg,
				WithSteps(newRecorder().steps()),
				WithHolder([]string{"runner-a", "runner-b"}[i]),
				WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
			)

			start.Wait()
			_, err := o.Apply(context.Background(), spec)

			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}()
	}

	start.Done()
	done.Wait()

	var succeeded, busy int
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrBusy):
			busy++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}

	// Whichever order they interleave in, both must not provision at once. The
	// loser is either refused, or runs afterwards and finds nothing to do.
	if succeeded+busy != 2 {
		t.Fatalf("outcomes = %v, want each run to either succeed or report busy", errs)
	}

	stored, err := reg.Get(t.Context(), spec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Phase != core.PhaseReady {
		t.Errorf("Phase = %s, want ready", stored.Phase)
	}
}

func TestApply_RefusesDecommissioningClusters(t *testing.T) {
	reg := registry.NewMemory()
	spec := testSpec()

	created, err := reg.Create(t.Context(), registry.NewRecord(spec, time.Now()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := reg.UpdatePhase(t.Context(), created, core.PhaseDecommissioning); err != nil {
		t.Fatalf("UpdatePhase: %v", err)
	}

	rec := newRecorder()
	if _, err := newOrchestrator(t, reg, rec.steps()).Apply(t.Context(), spec); !errors.Is(err, ErrDecommissioning) {
		t.Fatalf("error = %v, want one wrapping ErrDecommissioning", err)
	}
	if ran := rec.ran(); len(ran) != 0 {
		t.Errorf("steps ran = %v, want none on a decommissioning cluster", ran)
	}
}

func TestApply_RejectsAnInvalidSpec(t *testing.T) {
	reg := registry.NewMemory()
	spec := testSpec()
	spec.Region = ""

	rec := newRecorder()
	if _, err := newOrchestrator(t, reg, rec.steps()).Apply(t.Context(), spec); !errors.Is(err, core.ErrInvalidSpec) {
		t.Fatalf("error = %v, want one wrapping ErrInvalidSpec", err)
	}
	if ran := rec.ran(); len(ran) != 0 {
		t.Error("an invalid spec reached the steps")
	}
}

func TestApply_RegistersANewCluster(t *testing.T) {
	reg := registry.NewMemory()
	spec := testSpec()

	if _, err := newOrchestrator(t, reg, newRecorder().steps()).Apply(t.Context(), spec); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	stored, err := reg.Get(t.Context(), spec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Provider != spec.Provider || stored.Region != spec.Region || stored.Access != spec.Access {
		t.Errorf("record = %+v, want it to carry the spec's provider, region, and access", stored)
	}
}

// WithReadyReconcile is M3's split-diff idempotence: the phase state machine
// only ever runs each phase's step once, so a repeat `apply` against an
// already-ready cluster has to reconcile some other way, or drift after
// ready would never be noticed.
func TestApply_ReadyReconcile_RunsOnAFreshlyReadyCluster(t *testing.T) {
	reg := registry.NewMemory()
	spec := testSpec()

	var reconciled int
	o := New(reg,
		WithSteps(newRecorder().steps()),
		WithHolder("test-runner"),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithReadyReconcile(func(context.Context, core.ClusterSpec, registry.Record) error {
			reconciled++
			return nil
		}),
	)

	if _, err := o.Apply(t.Context(), spec); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if reconciled != 1 {
		t.Errorf("readyReconcile ran %d times, want 1", reconciled)
	}
}

func TestApply_ReadyReconcile_RunsOnAnAlreadyReadyCluster(t *testing.T) {
	reg := registry.NewMemory()
	spec := testSpec()

	// First apply reaches ready with no reconcile hook configured.
	if _, err := newOrchestrator(t, reg, newRecorder().steps()).Apply(t.Context(), spec); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	var reconciled int
	o := New(reg,
		WithSteps(newRecorder().steps()),
		WithHolder("test-runner"),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithReadyReconcile(func(context.Context, core.ClusterSpec, registry.Record) error {
			reconciled++
			return nil
		}),
	)

	if _, err := o.Apply(t.Context(), spec); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if reconciled != 1 {
		t.Errorf("readyReconcile ran %d times on an already-ready cluster, want 1", reconciled)
	}
}

func TestApply_ReadyReconcile_ErrorSurfaces(t *testing.T) {
	reg := registry.NewMemory()
	spec := testSpec()

	wantErr := errors.New("addon commit failed")
	o := New(reg,
		WithSteps(newRecorder().steps()),
		WithHolder("test-runner"),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithReadyReconcile(func(context.Context, core.ClusterSpec, registry.Record) error {
			return wantErr
		}),
	)

	if _, err := o.Apply(t.Context(), spec); !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want one wrapping %v", err, wantErr)
	}
}

func TestApply_ReadyReconcile_DoesNotRunWhenTheStepsFail(t *testing.T) {
	reg := registry.NewMemory()
	spec := testSpec()

	rec := newRecorder()
	rec.fail[core.PhasePending] = errors.New("boom")

	var reconciled int
	o := New(reg,
		WithSteps(rec.steps()),
		WithHolder("test-runner"),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithReadyReconcile(func(context.Context, core.ClusterSpec, registry.Record) error {
			reconciled++
			return nil
		}),
	)

	if _, err := o.Apply(t.Context(), spec); err == nil {
		t.Fatal("expected the step failure to surface")
	}
	if reconciled != 0 {
		t.Error("readyReconcile ran despite the cluster never reaching ready")
	}
}

func TestDefaultStepsCoverEveryProvisioningPhase(t *testing.T) {
	// A phase with no registered step would stall a run at exactly that point,
	// so the coverage is asserted rather than assumed.
	steps := DefaultSteps()

	for phase := core.PhasePending; ; {
		next, ok := phase.Next()
		if !ok || phase == core.PhaseReady {
			break
		}
		if _, registered := steps[phase]; !registered {
			t.Errorf("no default step registered for phase %s", phase)
		}
		phase = next
	}

	if _, registered := steps[core.PhaseReady]; registered {
		t.Error("ready has a step registered; nothing runs after ready")
	}
}

func TestDefaultHolderIsNonEmpty(t *testing.T) {
	// Two runs sharing a holder would each believe they own the lease.
	if defaultHolder() == "" {
		t.Error("defaultHolder returned an empty identity")
	}
}

// stepDiff describes how the steps that ran differ from what was expected,
// or returns "" when they match.
func stepDiff(got, want []string) string {
	if slices.Equal(got, want) {
		return ""
	}
	return fmt.Sprintf("got %v, want %v", got, want)
}
