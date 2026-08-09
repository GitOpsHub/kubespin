package core

import (
	"errors"
	"testing"
)

// allowed is the complete set of legal transitions, written out longhand rather
// than derived from the implementation. Asserting the full cartesian product of
// phases against this set is what makes the test meaningful: a bug that widens
// the transition table fails here instead of passing a rule it shares with the
// code under test.
func allowedTransitions() map[Phase]map[Phase]bool {
	live := []Phase{
		PhasePending, PhaseClusterCreated, PhaseIdentityBound,
		PhaseRepoPushed, PhaseArgoCDInstalled, PhaseReady,
	}

	allowed := map[Phase]map[Phase]bool{}
	for _, p := range PhaseOrder {
		allowed[p] = map[Phase]bool{p: true} // self-transition is always legal
	}

	// Single forward step on the happy path.
	forward := [][2]Phase{
		{PhasePending, PhaseClusterCreated},
		{PhaseClusterCreated, PhaseIdentityBound},
		{PhaseIdentityBound, PhaseRepoPushed},
		{PhaseRepoPushed, PhaseArgoCDInstalled},
		{PhaseArgoCDInstalled, PhaseReady},
		{PhaseDecommissioning, PhaseDecommissioned},
	}
	for _, f := range forward {
		allowed[f[0]][f[1]] = true
	}

	// Teardown is reachable from every live phase.
	for _, p := range live {
		allowed[p][PhaseDecommissioning] = true
	}

	return allowed
}

func TestCanTransition_ExhaustivePairs(t *testing.T) {
	allowed := allowedTransitions()

	for _, from := range PhaseOrder {
		for _, to := range PhaseOrder {
			want := allowed[from][to]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestCanTransition_RejectsUnknownPhases(t *testing.T) {
	unknown := Phase("banana")

	if CanTransition(unknown, PhaseReady) {
		t.Error("transition from an unknown phase must be rejected")
	}
	if CanTransition(PhasePending, unknown) {
		t.Error("transition to an unknown phase must be rejected")
	}
	if unknown.Valid() {
		t.Error("unknown phase reported itself valid")
	}
}

func TestCanTransition_NoSkippingOrRollback(t *testing.T) {
	if CanTransition(PhasePending, PhaseReady) {
		t.Error("skipping the state machine must be rejected")
	}
	if CanTransition(PhaseReady, PhaseArgoCDInstalled) {
		t.Error("rolling the state machine backwards must be rejected")
	}
	if CanTransition(PhaseDecommissioned, PhaseDecommissioning) {
		t.Error("a terminal phase must not transition anywhere but itself")
	}
}

func TestValidateTransition_WrapsSentinel(t *testing.T) {
	err := ValidateTransition(PhasePending, PhaseReady)
	if err == nil {
		t.Fatal("expected an error for an illegal transition")
	}
	if !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("error %v does not wrap ErrInvalidTransition", err)
	}
	if err := ValidateTransition(PhasePending, PhaseClusterCreated); err != nil {
		t.Errorf("legal transition returned %v", err)
	}
}

func TestPhaseNext(t *testing.T) {
	for _, tc := range []struct {
		from Phase
		want Phase
		ok   bool
	}{
		{PhasePending, PhaseClusterCreated, true},
		{PhaseArgoCDInstalled, PhaseReady, true},
		{PhaseDecommissioned, "", false},
	} {
		got, ok := tc.from.Next()
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s.Next() = (%s, %v), want (%s, %v)", tc.from, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPhaseOrderCoversEveryPhase(t *testing.T) {
	// Guards against a new phase constant being added without being registered
	// in PhaseOrder, which would silently shrink the exhaustive test above.
	seen := map[Phase]bool{}
	for _, p := range PhaseOrder {
		if seen[p] {
			t.Errorf("phase %s appears twice in PhaseOrder", p)
		}
		seen[p] = true
		if !p.Valid() {
			t.Errorf("phase %s in PhaseOrder is not Valid()", p)
		}
	}
	// Every phase that appears in the transition table must be registered in
	// PhaseOrder, in either position.
	for from, to := range forwardTransitions {
		if !seen[from] {
			t.Errorf("phase %s appears in the transition table but not in PhaseOrder", from)
		}
		if !seen[to] {
			t.Errorf("phase %s appears in the transition table but not in PhaseOrder", to)
		}
	}
}
