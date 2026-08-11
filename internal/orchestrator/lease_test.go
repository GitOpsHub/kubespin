package orchestrator

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/registry"
)

// countingRegistry wraps a Registry and reports how many times the lease was
// renewed, so a test can prove renewal happens *during* a step rather than
// only at the step boundaries.
type countingRegistry struct {
	registry.Registry

	renewals atomic.Int64

	mu       sync.Mutex
	renewErr error // returned by every RenewLease once set
}

func (r *countingRegistry) RenewLease(
	ctx context.Context, id core.ClusterID, holder string, ttl time.Duration,
) (registry.Lease, error) {
	r.renewals.Add(1)

	r.mu.Lock()
	err := r.renewErr
	r.mu.Unlock()

	if err != nil {
		return registry.Lease{}, err
	}
	return r.Registry.RenewLease(ctx, id, holder, ttl)
}

func (r *countingRegistry) failRenewals(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.renewErr = err
}

// leaseTestOrchestrator builds an orchestrator whose lease TTL is short enough
// that the heartbeat ticks several times inside a step that takes milliseconds.
func leaseTestOrchestrator(t *testing.T, reg registry.Registry, steps map[core.Phase]Step) *Orchestrator {
	t.Helper()

	o := newOrchestrator(t, reg, steps)
	WithLeaseTTL(150 * time.Millisecond)(o) // heartbeat every 50ms
	return o
}

// slowStepDuration outlasts several heartbeat intervals, standing in for the
// control-plane wait that outlasts the TTL in production.
const slowStepDuration = 400 * time.Millisecond

// TestApply_RenewsTheLeaseDuringALongStep is the regression test for the bug
// this heartbeat exists to fix: a step may legitimately run far longer than
// the lease TTL (a control plane takes 10-30 minutes against a 15-minute
// TTL), and renewing only between steps let the lease lapse mid-step.
func TestApply_RenewsTheLeaseDuringALongStep(t *testing.T) {
	reg := &countingRegistry{Registry: registry.NewMemory()}

	// One step that outlasts several heartbeat intervals.
	steps := DefaultSteps()
	steps[core.PhasePending] = StepFunc{
		Label: "create cluster",
		Fn: func(context.Context, core.ClusterSpec, registry.Record) error {
			time.Sleep(slowStepDuration)
			return nil
		},
	}

	o := leaseTestOrchestrator(t, reg, steps)

	before := reg.renewals.Load()
	if _, err := o.Apply(context.Background(), testSpec()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Five steps each renew once at their boundary; the heartbeat adds the
	// renewals that happened while the slow step was running. Without it the
	// count would be exactly the number of steps.
	renewals := reg.renewals.Load() - before
	if renewals <= int64(len(core.PhaseOrder)) {
		t.Errorf("lease renewed %d times; want more than the %d step-boundary renewals, "+
			"which means the heartbeat never ran during the slow step",
			renewals, len(core.PhaseOrder))
	}
}

// TestApply_AbortsWhenTheLeaseIsLost proves a run that can no longer prove it
// owns the cluster stops instead of continuing to mutate cloud resources
// another run may now own.
func TestApply_AbortsWhenTheLeaseIsLost(t *testing.T) {
	reg := &countingRegistry{Registry: registry.NewMemory()}

	stepRunning := make(chan struct{})
	steps := DefaultSteps()
	steps[core.PhasePending] = StepFunc{
		Label: "create cluster",
		Fn: func(ctx context.Context, _ core.ClusterSpec, _ registry.Record) error {
			close(stepRunning)
			// A real step is a cloud call that honours its context; block until
			// the heartbeat notices the lease is gone and cancels the run.
			<-ctx.Done()
			return ctx.Err()
		},
	}

	o := leaseTestOrchestrator(t, reg, steps)

	go func() {
		<-stepRunning
		reg.failRenewals(registry.ErrLeaseLost)
	}()

	_, err := o.Apply(context.Background(), testSpec())
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Apply error = %v, want it to wrap ErrLeaseLost", err)
	}
}

// TestApply_RidesOutATransientRenewalFailure keeps the heartbeat from being
// hair-trigger: a throttled or briefly unreachable registry must not kill a
// provisioning run while the lease already held is still valid.
func TestApply_RidesOutATransientRenewalFailure(t *testing.T) {
	reg := &countingRegistry{Registry: registry.NewMemory()}

	steps := DefaultSteps()
	steps[core.PhasePending] = StepFunc{
		Label: "create cluster",
		Fn: func(context.Context, core.ClusterSpec, registry.Record) error {
			// Fail one heartbeat's worth of renewals, then recover — well
			// inside the TTL, so the lease was never actually lost.
			reg.failRenewals(errors.New("dynamodb: throttled"))
			time.Sleep(75 * time.Millisecond)
			reg.failRenewals(nil)
			return nil
		},
	}

	o := leaseTestOrchestrator(t, reg, steps)

	if _, err := o.Apply(context.Background(), testSpec()); err != nil {
		t.Fatalf("Apply: %v, want the transient renewal failure to be ridden out", err)
	}
}

// TestDelete_RenewsTheLeaseDuringTeardown covers the same hazard on the
// teardown path, where waiting for a cloud to finish deleting a cluster is
// just as long as waiting for it to create one.
func TestDelete_RenewsTheLeaseDuringTeardown(t *testing.T) {
	reg := &countingRegistry{Registry: registry.NewMemory()}
	spec := testSpec()

	if _, err := reg.Create(context.Background(), registry.NewRecord(spec, time.Now())); err != nil {
		t.Fatalf("seeding registry: %v", err)
	}

	o := leaseTestOrchestrator(t, reg, DefaultSteps())

	before := reg.renewals.Load()
	teardown := func(context.Context, core.ClusterSpec, registry.Record) error {
		time.Sleep(slowStepDuration)
		return nil
	}

	if _, err := o.Delete(context.Background(), spec, teardown); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if renewals := reg.renewals.Load() - before; renewals == 0 {
		t.Error("lease was never renewed during teardown; the heartbeat did not run")
	}
}
