package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/registry"
)

// TeardownFunc performs the actual cleanup for a decommissioning cluster:
// identity deprovisioning, cluster deletion, repository archival. It runs
// once the cluster is recorded PhaseDecommissioning, so a crashed teardown
// resumes as a teardown on retry rather than an ordinary `apply`.
type TeardownFunc func(ctx context.Context, spec core.ClusterSpec, rec registry.Record) error

// Delete tears a cluster down in reverse order of Apply: mark
// PhaseDecommissioning in the registry, run teardown, mark
// PhaseDecommissioned.
//
// It is idempotent the same way Apply is: a cluster already decommissioned
// is a no-op, and a cluster already decommissioning resumes teardown rather
// than re-marking it — a retried `delete` is the same code path as the first
// one, not a special case.
func (o *Orchestrator) Delete(ctx context.Context, spec core.ClusterSpec, teardown TeardownFunc) (registry.Record, error) {
	rec, err := o.registry.Get(ctx, spec.ID)
	if err != nil {
		return registry.Record{}, fmt.Errorf("reading %s: %w", spec.ID, err)
	}
	if rec.Phase == core.PhaseDecommissioned {
		o.logger.Info("cluster is already decommissioned", "cluster", spec.ID)
		return rec, nil
	}

	if _, err := o.registry.AcquireLease(ctx, spec.ID, o.holder, o.leaseTTL); err != nil {
		if errors.Is(err, registry.ErrLeaseHeld) {
			return rec, fmt.Errorf("%w: %s", ErrBusy, spec.ID)
		}
		return rec, fmt.Errorf("acquiring lease on %s: %w", spec.ID, err)
	}
	defer o.release(ctx, spec.ID)

	// Teardown blocks on the cloud actually deleting the cluster, which takes
	// longer than the lease TTL just as creation does — so it runs under the
	// same heartbeat Apply uses. See keepLeaseAlive.
	runCtx, stopHeartbeat := o.keepLeaseAlive(ctx, spec.ID)
	defer stopHeartbeat()

	// Re-read under the lease: another run may have advanced (or finished)
	// teardown between the first read and acquiring it.
	if rec, err = o.registry.Get(runCtx, spec.ID); err != nil {
		return rec, leaseFailure(runCtx, fmt.Errorf("reading %s: %w", spec.ID, err))
	}
	if rec.Phase == core.PhaseDecommissioned {
		return rec, nil
	}

	if rec.Phase != core.PhaseDecommissioning {
		if rec, err = o.registry.UpdatePhase(runCtx, rec, core.PhaseDecommissioning); err != nil {
			return rec, leaseFailure(runCtx, fmt.Errorf("marking %s decommissioning: %w", spec.ID, err))
		}
		o.logger.Info("marked cluster decommissioning", "cluster", spec.ID)
	}

	if err := teardown(runCtx, spec, rec); err != nil {
		// The phase is deliberately left at decommissioning: a retried delete
		// resumes teardown rather than believing the cluster is still live.
		return rec, leaseFailure(runCtx, fmt.Errorf("tearing down %s: %w", spec.ID, err))
	}

	updated, err := o.registry.UpdatePhase(runCtx, rec, core.PhaseDecommissioned)
	if err != nil {
		return rec, leaseFailure(runCtx, fmt.Errorf("marking %s decommissioned: %w", spec.ID, err))
	}
	o.logger.Info("cluster decommissioned", "cluster", spec.ID)
	return updated, nil
}
