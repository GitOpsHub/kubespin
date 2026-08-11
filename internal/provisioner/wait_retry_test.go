package provisioner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// flakyProvisioner fails the first failures Describe calls, then reports
// states from a script — the shape of a cloud API that throttles briefly
// during a half-hour poll and then carries on.
type flakyProvisioner struct {
	failures int // remaining calls that will fail
	err      error
	states   []ClusterState

	calls int
}

func (p *flakyProvisioner) Provider() core.Provider                        { return core.ProviderAWS }
func (p *flakyProvisioner) Create(context.Context, core.ClusterSpec) error { return nil }
func (p *flakyProvisioner) Delete(context.Context, core.ClusterSpec) error { return nil }

func (p *flakyProvisioner) Reconcile(context.Context, core.ClusterSpec) (Change, error) {
	return Change{}, nil
}

func (p *flakyProvisioner) Describe(context.Context, core.ClusterSpec) (ClusterState, error) {
	p.calls++
	if p.failures > 0 {
		p.failures--
		return ClusterState{}, p.err
	}
	return p.states[min(p.calls-1, len(p.states)-1)], nil
}

func retryWait(maxErrors int) WaitOptions {
	opts := fastWait()
	opts.MaxDescribeErrors = maxErrors
	return opts
}

// TestWaitUntilActive_RidesOutTransientDescribeErrors is the regression test
// for treating the first failed poll as fatal: a cluster that was being
// created perfectly well had its apply aborted by one throttled API call.
func TestWaitUntilActive_RidesOutTransientDescribeErrors(t *testing.T) {
	p := &flakyProvisioner{
		failures: 3,
		err:      errors.New("ThrottlingException: rate exceeded"),
		states:   []ClusterState{{Status: StatusActive, Endpoint: "https://example"}},
	}

	state, err := WaitUntilActive(context.Background(), p, core.ClusterSpec{ID: "c"}, retryWait(5))
	if err != nil {
		t.Fatalf("WaitUntilActive: %v, want the throttles to be ridden out", err)
	}
	if state.Status != StatusActive {
		t.Errorf("Status = %s, want active", state.Status)
	}
}

// TestWaitUntilActive_FailsOnPersistentDescribeErrors keeps the retry from
// swallowing a genuine outage: it must still give up, and say why.
func TestWaitUntilActive_FailsOnPersistentDescribeErrors(t *testing.T) {
	boom := errors.New("AccessDeniedException")
	p := &flakyProvisioner{
		failures: 100,
		err:      boom,
		states:   []ClusterState{{Status: StatusActive}},
	}

	_, err := WaitUntilActive(context.Background(), p, core.ClusterSpec{ID: "c"}, retryWait(4))
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap the underlying failure", err)
	}
	if !strings.Contains(err.Error(), "consecutive") {
		t.Errorf("error = %q, want it to say the failures were consecutive", err)
	}
	if p.calls != 4 {
		t.Errorf("Describe called %d times, want exactly the 4 tolerated attempts", p.calls)
	}
}

// TestWaitUntilActive_ResetsTheFailureCountOnSuccess proves the tolerance is
// for *consecutive* failures. Occasional blips spread across a long poll must
// never accumulate into a spurious give-up.
func TestWaitUntilActive_ResetsTheFailureCountOnSuccess(t *testing.T) {
	// fail, ok, fail, ok, ... with a tolerance of 2: this only completes if a
	// successful poll clears the count.
	p := &alternatingProvisioner{
		err:   errors.New("connection reset by peer"),
		final: 8,
	}

	if _, err := WaitUntilActive(context.Background(), p, core.ClusterSpec{ID: "c"}, retryWait(2)); err != nil {
		t.Fatalf("WaitUntilActive: %v, want intermittent failures to never accumulate", err)
	}
}

// alternatingProvisioner fails every other Describe until call `final`, which
// reports the cluster active.
type alternatingProvisioner struct {
	err   error
	final int
	calls int
}

func (p *alternatingProvisioner) Provider() core.Provider                        { return core.ProviderAWS }
func (p *alternatingProvisioner) Create(context.Context, core.ClusterSpec) error { return nil }
func (p *alternatingProvisioner) Delete(context.Context, core.ClusterSpec) error { return nil }

func (p *alternatingProvisioner) Reconcile(context.Context, core.ClusterSpec) (Change, error) {
	return Change{}, nil
}

func (p *alternatingProvisioner) Describe(context.Context, core.ClusterSpec) (ClusterState, error) {
	p.calls++
	if p.calls >= p.final {
		return ClusterState{Status: StatusActive}, nil
	}
	if p.calls%2 == 1 {
		return ClusterState{}, p.err
	}
	return ClusterState{Status: StatusCreating}, nil
}

// TestWaitUntilGone_RidesOutTransientDescribeErrors covers the teardown side,
// where an aborted wait leaves the registry claiming a cluster is still live.
func TestWaitUntilGone_RidesOutTransientDescribeErrors(t *testing.T) {
	p := &flakyProvisioner{
		failures: 2,
		err:      errors.New("ThrottlingException: rate exceeded"),
		states:   []ClusterState{{Status: StatusAbsent}},
	}

	if err := WaitUntilGone(context.Background(), p, core.ClusterSpec{ID: "c"}, retryWait(5)); err != nil {
		t.Fatalf("WaitUntilGone: %v, want the throttles to be ridden out", err)
	}
}

// TestWaitUntilActive_CancellationIsNotTransient keeps an interrupted run
// from being retried: the operator pressed Ctrl-C, and no number of further
// polls will change that.
func TestWaitUntilActive_CancellationIsNotTransient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := &flakyProvisioner{
		failures: 100,
		err:      context.Canceled,
		states:   []ClusterState{{Status: StatusActive}},
	}

	_, err := WaitUntilActive(ctx, p, core.ClusterSpec{ID: "c"}, retryWait(5))
	if err == nil {
		t.Fatal("WaitUntilActive returned nil, want a cancellation error")
	}
	if p.calls != 1 {
		t.Errorf("Describe called %d times, want it to stop after the first", p.calls)
	}
}
