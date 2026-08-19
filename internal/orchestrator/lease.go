package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/registry"
)

// ErrLeaseLost is returned when a run can no longer prove it holds the
// cluster's lease. It is deliberately fatal to the run: once the lease is
// gone another `apply` may already be provisioning the same cluster, and two
// runs mutating one cluster's cloud resources is exactly what the lease
// exists to prevent.
var ErrLeaseLost = errors.New("lost the cluster lease mid-run")

// leaseRenewalDivisor sets the heartbeat interval as a fraction of the TTL.
// Renewing three times per TTL leaves room for two consecutive failed
// renewals — a throttled or briefly unreachable Postgres — before the lease
// genuinely expires.
const leaseRenewalDivisor = 3

// minRenewalInterval only guards against a zero or negative TTL producing a
// busy loop. It must stay well below any realistic TTL: a floor that exceeds
// TTL/leaseRenewalDivisor would renew *after* expiry, reintroducing exactly
// the lapse this file exists to prevent.
const minRenewalInterval = 10 * time.Millisecond

// keepLeaseAlive renews id's lease in the background until the returned stop
// function is called, and returns a context that is cancelled the moment the
// lease can no longer be proven held.
//
// This exists because the lease TTL only bounds how long a *crashed* run can
// block a cluster; it cannot also bound how long a step may legitimately
// take. A single step — waiting for a control plane on any of the three
// clouds — routinely runs 10 to 30 minutes and is allowed up to
// WaitOptions.Timeout (45 minutes by default), which is three times
// DefaultLeaseTTL. Renewing only between steps therefore let the lease lapse
// mid-step, with two consequences that both showed up as the same symptom:
// another apply could acquire the "expired" lease and provision the same
// cluster concurrently, and this run would then fail its next between-steps
// renewal with ErrLeaseLost after having already done all the work.
//
// Renewing on a timer instead decouples the TTL from step duration: the TTL
// now only has to outlast a few failed renewals, not the longest step.
func (o *Orchestrator) keepLeaseAlive(ctx context.Context, id core.ClusterID) (context.Context, func()) {
	runCtx, cancel := context.WithCancelCause(ctx)

	interval := max(o.leaseTTL/leaseRenewalDivisor, minRenewalInterval)

	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// The last moment this run can prove it owns the lease. Renewal
		// failures are tolerated until it passes, then the run must stop:
		// past expiry another holder may legitimately have taken over.
		expiry := o.now().Add(o.leaseTTL)

		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}

			lease, err := o.registry.RenewLease(runCtx, id, o.holder, o.leaseTTL)
			switch {
			case err == nil:
				expiry = lease.ExpiresAt
				o.logger.Debug("renewed lease", "cluster", id, "holder", o.holder, "expires_at", expiry)

			case runCtx.Err() != nil:
				// The run finished or the operator interrupted it; the
				// deferred release is what cleans up, not this goroutine.
				return

			case errors.Is(err, registry.ErrLeaseLost), errors.Is(err, registry.ErrNotFound):
				// Unambiguous: someone else holds it, or the record is gone.
				// No amount of retrying recovers this.
				cancel(fmt.Errorf("%w: %s: %w", ErrLeaseLost, id, err))
				return

			default:
				// Transient — a throttle or a network blip. Keep trying while
				// the lease we already hold is still valid.
				if remaining := expiry.Sub(o.now()); remaining > 0 {
					o.logger.Warn("could not renew lease; retrying while the current one is still valid",
						"cluster", id, "holder", o.holder, "valid_for", remaining, "error", err)
					continue
				}
				cancel(fmt.Errorf("%w: %s: renewal kept failing until the lease expired: %w",
					ErrLeaseLost, id, err))
				return
			}
		}
	}()

	return runCtx, func() {
		cancel(nil)
		<-done
	}
}

// leaseFailure reports a lost lease as the run's cause of death.
//
// Once keepLeaseAlive cancels the run context, every in-flight cloud call
// fails with a context error, so err on its own reads as an unexplained
// cancellation. Surfacing the lease loss instead is what makes the failure
// actionable ("another run took over") rather than mystifying.
func leaseFailure(runCtx context.Context, err error) error {
	cause := context.Cause(runCtx)
	if cause == nil || !errors.Is(cause, ErrLeaseLost) {
		return err
	}
	return fmt.Errorf("%w (run stopped at: %w)", cause, err)
}
