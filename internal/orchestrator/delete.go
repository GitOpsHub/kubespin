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
// PhaseDecommissioning in the registry, run teardown, then remove the
// cluster's registry record entirely. The registry is a resume log, not an
// inventory, so once teardown has finished there is nothing left to resume.
// Keeping a tombstone would only block a later apply from reusing the ID.
//
// The returned record carries PhaseDecommissioned for reporting, but it is
// never persisted. A cluster already decommissioning resumes teardown rather
// than being re-marked, so a retried `delete` runs the same code path as the
// first one. A record an older binary left at PhaseDecommissioned is removed
// without running teardown again. Once the record is gone, the ID is unknown
// to the registry, and a further delete fails with registry.ErrNotFound.
func (o *Orchestrator) Delete(ctx context.Context, spec core.ClusterSpec, teardown TeardownFunc) (registry.Record, error) {
	rec, err := o.registry.Get(ctx, spec.ID)
	if err != nil {
		return registry.Record{}, fmt.Errorf("reading %s: %w", spec.ID, err)
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
		// Only an older binary leaves this tombstone behind. Removing it
		// under the lease stops a concurrent apply that is reviving the same
		// ID from having its new row deleted.
		o.logger.Info("Removing Decommissioned Record", "cluster", spec.ID)
		return o.forget(runCtx, rec)
	}

	if rec.Phase != core.PhaseDecommissioning {
		if rec, err = o.registry.UpdatePhase(runCtx, rec, core.PhaseDecommissioning); err != nil {
			return rec, leaseFailure(runCtx, fmt.Errorf("marking %s decommissioning: %w", spec.ID, err))
		}
		o.logger.Info("Marked Cluster Decommissioning", "cluster", spec.ID)
	}

	if err := teardown(runCtx, spec, rec); err != nil {
		// The phase is deliberately left at decommissioning: a retried delete
		// resumes teardown rather than believing the cluster is still live.
		return rec, leaseFailure(runCtx, fmt.Errorf("tearing down %s: %w", spec.ID, err))
	}

	done, err := o.forget(runCtx, rec)
	if err != nil {
		return rec, leaseFailure(runCtx, err)
	}
	o.logger.Info("Cluster Decommissioned", "cluster", spec.ID)
	return done, nil
}

// forget removes rec from the registry once its teardown is complete, and
// returns it with PhaseDecommissioned so the caller can report the outcome.
// Deleting the row drops the lease along with it, which makes the deferred
// release a no-op.
func (o *Orchestrator) forget(ctx context.Context, rec registry.Record) (registry.Record, error) {
	if err := o.registry.Delete(ctx, rec.ClusterID); err != nil {
		return rec, fmt.Errorf("removing registry record for %s: %w", rec.ClusterID, err)
	}
	rec.Phase = core.PhaseDecommissioned
	rec.Lease = nil
	return rec, nil
}
