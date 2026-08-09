package provisioner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// scriptedProvisioner returns a prepared sequence of states, one per Describe.
type scriptedProvisioner struct {
	states []ClusterState
	calls  int
	err    error
}

func (p *scriptedProvisioner) Provider() core.Provider                        { return core.ProviderAWS }
func (p *scriptedProvisioner) Create(context.Context, core.ClusterSpec) error { return nil }
func (p *scriptedProvisioner) Delete(context.Context, core.ClusterSpec) error { return nil }

func (p *scriptedProvisioner) Reconcile(context.Context, core.ClusterSpec) (Change, error) {
	return Change{}, nil
}

func (p *scriptedProvisioner) Describe(context.Context, core.ClusterSpec) (ClusterState, error) {
	if p.err != nil {
		return ClusterState{}, p.err
	}

	state := p.states[min(p.calls, len(p.states)-1)]
	p.calls++
	return state, nil
}

func fastWait() WaitOptions {
	return WaitOptions{Interval: time.Millisecond, Timeout: time.Second}
}

func TestWaitUntilActive(t *testing.T) {
	t.Run("polls until active", func(t *testing.T) {
		p := &scriptedProvisioner{states: []ClusterState{
			// Absent first: the cloud may not have registered the cluster yet,
			// which is normal rather than a failure.
			{Status: StatusAbsent},
			{Status: StatusCreating},
			{Status: StatusCreating},
			{Status: StatusActive, Endpoint: "https://example"},
		}}

		state, err := WaitUntilActive(context.Background(), p, core.ClusterSpec{ID: "c"}, fastWait())
		if err != nil {
			t.Fatalf("WaitUntilActive: %v", err)
		}
		if state.Status != StatusActive {
			t.Errorf("Status = %s, want active", state.Status)
		}
		if p.calls < 4 {
			t.Errorf("Describe called %d times, want it to poll through every state", p.calls)
		}
	})

	// Waiting will never clear a failed cluster, so it must not burn the timeout.
	t.Run("fails fast on a failed cluster", func(t *testing.T) {
		p := &scriptedProvisioner{states: []ClusterState{{Status: StatusFailed}}}

		_, err := WaitUntilActive(context.Background(), p, core.ClusterSpec{ID: "c"}, fastWait())
		if !errors.Is(err, ErrClusterFailed) {
			t.Fatalf("error = %v, want one wrapping ErrClusterFailed", err)
		}
		if p.calls != 1 {
			t.Errorf("Describe called %d times, want it to stop immediately", p.calls)
		}
	})

	t.Run("times out with the last status", func(t *testing.T) {
		p := &scriptedProvisioner{states: []ClusterState{{Status: StatusCreating}}}

		_, err := WaitUntilActive(context.Background(), p, core.ClusterSpec{ID: "c"},
			WaitOptions{Interval: time.Millisecond, Timeout: 10 * time.Millisecond})
		if err == nil {
			t.Fatal("expected a timeout")
		}
		if !strings.Contains(err.Error(), "creating") {
			t.Errorf("error %q does not report the last observed status", err)
		}
	})

	t.Run("honours cancellation", func(t *testing.T) {
		p := &scriptedProvisioner{states: []ClusterState{{Status: StatusCreating}}}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := WaitUntilActive(ctx, p, core.ClusterSpec{ID: "c"}, fastWait()); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})

	t.Run("surfaces describe errors", func(t *testing.T) {
		sentinel := errors.New("throttled")
		p := &scriptedProvisioner{err: sentinel}

		if _, err := WaitUntilActive(context.Background(), p, core.ClusterSpec{ID: "c"}, fastWait()); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want the describe error", err)
		}
	})
}

func TestStatusSettled(t *testing.T) {
	for status, want := range map[Status]bool{
		StatusActive:   true,
		StatusFailed:   true,
		StatusAbsent:   true,
		StatusCreating: false,
		StatusUpdating: false,
		StatusDeleting: false,
	} {
		if got := status.Settled(); got != want {
			t.Errorf("%s.Settled() = %v, want %v", status, got, want)
		}
	}
}

func TestChangeMerge(t *testing.T) {
	change := Change{}
	change.Merge(Change{})
	if change.Changed {
		t.Error("merging two empty changes reported a change")
	}

	change.Merge(Change{Changed: true, Details: []string{"resized"}})
	change.Merge(Change{Details: []string{"noted"}})

	if !change.Changed {
		t.Error("Changed was lost by a subsequent no-op merge")
	}
	if len(change.Details) != 2 {
		t.Errorf("Details = %v, want both merged in", change.Details)
	}
}

func TestStatusReporterComponent(t *testing.T) {
	comp := StatusReporter()

	if comp.Name == "" || comp.Namespace == "" || comp.ServiceAccount == "" {
		t.Errorf("component = %+v, want every field populated", comp)
	}
}

func TestWaitUntilGone(t *testing.T) {
	t.Run("polls until the cluster is absent", func(t *testing.T) {
		p := &scriptedProvisioner{states: []ClusterState{
			{Status: StatusActive},
			{Status: StatusDeleting},
			{Status: StatusDeleting},
			{Status: StatusAbsent},
		}}

		if err := WaitUntilGone(context.Background(), p, core.ClusterSpec{ID: "c"}, fastWait()); err != nil {
			t.Fatalf("WaitUntilGone: %v", err)
		}
		if p.calls < 4 {
			t.Errorf("Describe called %d times, want it to poll until absent", p.calls)
		}
	})

	// A deletion the cloud gave up on will not clear itself.
	t.Run("fails fast on a failed cluster", func(t *testing.T) {
		p := &scriptedProvisioner{states: []ClusterState{{Status: StatusFailed}}}

		err := WaitUntilGone(context.Background(), p, core.ClusterSpec{ID: "c"}, fastWait())
		if !errors.Is(err, ErrClusterFailed) {
			t.Fatalf("error = %v, want one wrapping ErrClusterFailed", err)
		}
		if p.calls != 1 {
			t.Errorf("Describe called %d times, want it to stop immediately", p.calls)
		}
	})

	t.Run("times out on a cluster that never goes away", func(t *testing.T) {
		p := &scriptedProvisioner{states: []ClusterState{{Status: StatusDeleting}}}

		err := WaitUntilGone(context.Background(), p, core.ClusterSpec{ID: "c"},
			WaitOptions{Interval: time.Millisecond, Timeout: 10 * time.Millisecond})
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("error = %v, want a timeout", err)
		}
	})
}
