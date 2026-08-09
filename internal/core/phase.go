package core

import (
	"errors"
	"fmt"
)

// ErrInvalidTransition is returned when a phase change is not legal. The Fleet
// Registry checks this on every write, so an illegal state machine move fails at
// the storage boundary instead of being silently persisted.
var ErrInvalidTransition = errors.New("invalid phase transition")

// Phase is a cluster's position in the provisioning state machine. The
// orchestrator resumes a failed apply by re-entering at the recorded phase,
// which is what makes retry and first run the same code path.
type Phase string

// The provisioning state machine, in order.
const (
	PhasePending         Phase = "pending"
	PhaseClusterCreated  Phase = "cluster-created"
	PhaseIdentityBound   Phase = "identity-bound"
	PhaseRepoPushed      Phase = "repo-pushed"
	PhaseArgoCDInstalled Phase = "argocd-installed"
	PhaseReady           Phase = "ready"

	// Teardown phases. Decommissioning is reachable from any live phase, so a
	// half-built cluster can still be deleted.
	PhaseDecommissioning Phase = "decommissioning"
	PhaseDecommissioned  Phase = "decommissioned"
)

// forwardTransitions is the happy path: each phase to its single successor.
var forwardTransitions = map[Phase]Phase{
	PhasePending:         PhaseClusterCreated,
	PhaseClusterCreated:  PhaseIdentityBound,
	PhaseIdentityBound:   PhaseRepoPushed,
	PhaseRepoPushed:      PhaseArgoCDInstalled,
	PhaseArgoCDInstalled: PhaseReady,
	PhaseDecommissioning: PhaseDecommissioned,
}

// PhaseOrder is every phase in state machine order, for display and iteration.
// It is the authoritative list of phases: Valid is derived from it, so a new
// phase constant is only recognised once it is registered here.
var PhaseOrder = []Phase{
	PhasePending, PhaseClusterCreated, PhaseIdentityBound,
	PhaseRepoPushed, PhaseArgoCDInstalled, PhaseReady,
	PhaseDecommissioning, PhaseDecommissioned,
}

var validPhases = func() map[Phase]struct{} {
	m := make(map[Phase]struct{}, len(PhaseOrder))
	for _, p := range PhaseOrder {
		m[p] = struct{}{}
	}
	return m
}()

// Valid reports whether p is a known phase.
//
// Validity comes from PhaseOrder, not from the transition table: terminal
// phases have no successor but are still perfectly valid phases to be in.
func (p Phase) Valid() bool {
	_, ok := validPhases[p]
	return ok
}

func (p Phase) String() string { return string(p) }

// Terminal reports whether no further transition is possible.
func (p Phase) Terminal() bool { return p == PhaseDecommissioned }

// Next returns the phase that follows p on the happy path.
func (p Phase) Next() (Phase, bool) {
	n, ok := forwardTransitions[p]
	return n, ok
}

// CanTransition reports whether from -> to is legal.
//
// Three rules, in order of precedence:
//   - A phase may always transition to itself. The orchestrator re-writes its
//     current phase on retry, and that must be an idempotent no-op rather than
//     an error.
//   - Any live phase may enter PhaseDecommissioning, so a cluster that failed
//     halfway through provisioning can still be torn down.
//   - Otherwise only the single forward step is legal: no skipping, no rollback.
func CanTransition(from, to Phase) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	if from == to {
		return true
	}
	if from.Terminal() {
		return false
	}
	if to == PhaseDecommissioning {
		return true
	}
	next, ok := forwardTransitions[from]
	return ok && next == to
}

// ValidateTransition returns an ErrInvalidTransition-wrapped error describing
// why from -> to is not allowed, or nil when it is.
func ValidateTransition(from, to Phase) error {
	if !from.Valid() {
		return fmt.Errorf("%w: %q is not a known phase", ErrInvalidTransition, from)
	}
	if !to.Valid() {
		return fmt.Errorf("%w: %q is not a known phase", ErrInvalidTransition, to)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	return nil
}
