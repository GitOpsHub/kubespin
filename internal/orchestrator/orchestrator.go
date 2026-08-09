// Package orchestrator sequences the provisioning of a single cluster.
//
// It is the piece that turns the phase state machine into an actual run:
// acquire the cluster's lease, then walk pending → cluster-created →
// identity-bound → repo-pushed → argocd-installed → ready, recording the phase
// in the Fleet Registry after each step.
//
// Two properties fall out of doing this here rather than inside the apply
// command, and both are hard to add later:
//
//   - Resumability. A failed run leaves the cluster at its last completed
//     phase. The next run reads that phase and re-enters there, so retry and
//     first run are the same code path rather than a special case.
//   - Testability. Every step is an interface, so the whole state machine is
//     exercised against fakes with no cloud credentials — which is what lets it
//     exist before any provisioner does.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/registry"
)

// ErrBusy is returned when another run holds the cluster's lease.
var ErrBusy = errors.New("cluster is being provisioned by another run")

// ErrDecommissioning is returned when apply is called on a cluster that is
// being or has been torn down. Reviving one is not a phase transition; it is a
// new cluster.
var ErrDecommissioning = errors.New("cluster is decommissioning or decommissioned")

// DefaultLeaseTTL bounds how long a crashed run can block a cluster. The
// orchestrator renews it before each step, so this only has to outlast the
// longest single step, not the whole run.
const DefaultLeaseTTL = 15 * time.Minute

// releaseTimeout bounds the best-effort lease release on the way out.
const releaseTimeout = 10 * time.Second

// Step performs the work that moves a cluster out of one phase.
//
// A step must be safe to re-run: a resumed apply re-executes the step for the
// phase it stopped at, because the phase is only recorded once the step
// succeeded.
type Step interface {
	Name() string
	Run(ctx context.Context, spec core.ClusterSpec, rec registry.Record) error
}

// StepFunc adapts a function to Step.
type StepFunc struct {
	Label string
	Fn    func(ctx context.Context, spec core.ClusterSpec, rec registry.Record) error
}

// Name returns the step's label.
func (s StepFunc) Name() string { return s.Label }

// Run invokes the wrapped function.
func (s StepFunc) Run(ctx context.Context, spec core.ClusterSpec, rec registry.Record) error {
	if s.Fn == nil {
		return nil
	}
	return s.Fn(ctx, spec, rec)
}

// DefaultSteps returns a no-op step for every phase.
//
// These are placeholders until the provisioners land: cluster and identity
// creation in M2, repository seeding in M3, and the Argo CD bootstrap in M5.
// Their names document what each phase is waiting on.
func DefaultSteps() map[core.Phase]Step {
	return map[core.Phase]Step{
		core.PhasePending:         StepFunc{Label: "create cluster"},
		core.PhaseClusterCreated:  StepFunc{Label: "bind workload identity"},
		core.PhaseIdentityBound:   StepFunc{Label: "create and seed repository"},
		core.PhaseRepoPushed:      StepFunc{Label: "install Argo CD"},
		core.PhaseArgoCDInstalled: StepFunc{Label: "verify addons healthy"},
	}
}

// ReconcileFunc reconciles a cluster that is already at PhaseReady.
type ReconcileFunc func(ctx context.Context, spec core.ClusterSpec, rec registry.Record) error

// Orchestrator drives one cluster's provisioning at a time.
type Orchestrator struct {
	registry       registry.Registry
	steps          map[core.Phase]Step
	holder         string
	leaseTTL       time.Duration
	now            func() time.Time
	logger         *slog.Logger
	readyReconcile ReconcileFunc
}

// Option configures an Orchestrator.
type Option func(*Orchestrator)

// WithSteps replaces the phase steps.
func WithSteps(steps map[core.Phase]Step) Option {
	return func(o *Orchestrator) { o.steps = steps }
}

// WithHolder sets the lease holder identity. It must be unique per run —
// two runs sharing a holder would each believe they own the lease.
func WithHolder(holder string) Option {
	return func(o *Orchestrator) { o.holder = holder }
}

// WithLeaseTTL sets how long the lease is held between renewals.
func WithLeaseTTL(ttl time.Duration) Option {
	return func(o *Orchestrator) { o.leaseTTL = ttl }
}

// WithClock replaces the time source.
func WithClock(now func() time.Time) Option {
	return func(o *Orchestrator) { o.now = now }
}

// WithLogger sets the logger.
func WithLogger(logger *slog.Logger) Option {
	return func(o *Orchestrator) { o.logger = logger }
}

// WithReadyReconcile sets the function Apply runs whenever it leaves a
// cluster at PhaseReady — whether that is the outcome of this call's phase
// walk or the cluster was already ready when Apply was called.
//
// This is where `apply`'s split-diff idempotence lives once a cluster exists:
// the phase state machine only ever runs each phase's step once, so ongoing
// infra reconciliation (M2's ClusterProvisioner.Reconcile) and addon
// reconciliation (M3's repo.ReconcileAddons) both have to happen here rather
// than as a phase step, or a repeat `apply` against a ready cluster would
// never notice drift.
func WithReadyReconcile(fn ReconcileFunc) Option {
	return func(o *Orchestrator) { o.readyReconcile = fn }
}

// New builds an orchestrator over reg.
func New(reg registry.Registry, opts ...Option) *Orchestrator {
	o := &Orchestrator{
		registry: reg,
		steps:    DefaultSteps(),
		holder:   defaultHolder(),
		leaseTTL: DefaultLeaseTTL,
		now:      time.Now,
		logger:   slog.Default(),
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// defaultHolder identifies this run well enough to debug a stuck lease.
func defaultHolder() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

// Apply drives spec's cluster to ready, resuming from whatever phase it is in.
//
// It is idempotent: a cluster already at ready runs no steps and writes
// nothing. A cluster part-way through resumes at its recorded phase.
func (o *Orchestrator) Apply(ctx context.Context, spec core.ClusterSpec) (registry.Record, error) {
	if err := spec.Validate(); err != nil {
		return registry.Record{}, fmt.Errorf("validating spec for %s: %w", spec.ID, err)
	}

	rec, err := o.ensureRecord(ctx, spec)
	if err != nil {
		return registry.Record{}, err
	}

	if _, err := o.registry.AcquireLease(ctx, spec.ID, o.holder, o.leaseTTL); err != nil {
		if errors.Is(err, registry.ErrLeaseHeld) {
			return rec, fmt.Errorf("%w: %s", ErrBusy, spec.ID)
		}
		return rec, fmt.Errorf("acquiring lease on %s: %w", spec.ID, err)
	}
	defer o.release(ctx, spec.ID)

	// Re-read under the lease. Between the first read and acquiring, another run
	// may have advanced the cluster — resuming from the earlier phase would
	// re-execute work that is already done.
	if rec, err = o.registry.Get(ctx, spec.ID); err != nil {
		return rec, fmt.Errorf("reading %s: %w", spec.ID, err)
	}

	rec, err = o.run(ctx, spec, rec)
	if err != nil {
		return rec, err
	}

	if rec.Phase == core.PhaseReady && o.readyReconcile != nil {
		if err := o.readyReconcile(ctx, spec, rec); err != nil {
			return rec, fmt.Errorf("reconciling %s: %w", spec.ID, err)
		}
	}
	return rec, nil
}

// ensureRecord returns the cluster's record, registering it if new.
func (o *Orchestrator) ensureRecord(ctx context.Context, spec core.ClusterSpec) (registry.Record, error) {
	rec, err := o.registry.Get(ctx, spec.ID)
	if err == nil {
		return rec, nil
	}
	if !errors.Is(err, registry.ErrNotFound) {
		return registry.Record{}, fmt.Errorf("reading %s: %w", spec.ID, err)
	}

	rec, err = o.registry.Create(ctx, registry.NewRecord(spec, o.now()))
	if err == nil {
		o.logger.Info("registered cluster", "cluster", spec.ID, "phase", core.PhasePending)
		return rec, nil
	}

	// Another run registered it between our read and our write. That is the
	// lease's job to arbitrate, not an error.
	if errors.Is(err, registry.ErrAlreadyExists) {
		rec, err = o.registry.Get(ctx, spec.ID)
		if err != nil {
			return registry.Record{}, fmt.Errorf("reading %s: %w", spec.ID, err)
		}
		return rec, nil
	}
	return registry.Record{}, fmt.Errorf("registering %s: %w", spec.ID, err)
}

// run walks the state machine from rec's current phase to ready.
func (o *Orchestrator) run(ctx context.Context, spec core.ClusterSpec, rec registry.Record) (registry.Record, error) {
	if rec.Phase == core.PhaseDecommissioning || rec.Phase == core.PhaseDecommissioned {
		return rec, fmt.Errorf("%w: %s is at phase %s", ErrDecommissioning, spec.ID, rec.Phase)
	}

	if rec.Phase == core.PhaseReady {
		o.logger.Info("cluster is already ready", "cluster", spec.ID)
		return rec, nil
	}

	o.logger.Info("provisioning cluster",
		"cluster", spec.ID, "resuming_from", rec.Phase, "provider", rec.Provider)

	// Bounded by the length of the state machine: a step that failed to advance
	// the phase must surface as an error rather than spin.
	for range len(core.PhaseOrder) {
		if rec.Phase == core.PhaseReady {
			return rec, nil
		}

		next, ok := rec.Phase.Next()
		if !ok {
			return rec, fmt.Errorf("no transition out of phase %s for %s", rec.Phase, spec.ID)
		}

		var err error
		if rec, err = o.advance(ctx, spec, rec, next); err != nil {
			return rec, err
		}
	}

	return rec, fmt.Errorf("cluster %s did not reach ready; stuck at %s", spec.ID, rec.Phase)
}

// advance runs the step for rec's phase and records the next phase.
func (o *Orchestrator) advance(
	ctx context.Context, spec core.ClusterSpec, rec registry.Record, next core.Phase,
) (registry.Record, error) {
	step, ok := o.steps[rec.Phase]
	if !ok {
		return rec, fmt.Errorf("no step registered for phase %s", rec.Phase)
	}

	// Renewed before each step rather than on a timer: steps are the long
	// operations, and a renewal that races a step boundary is the case that
	// matters.
	if _, err := o.registry.RenewLease(ctx, spec.ID, o.holder, o.leaseTTL); err != nil {
		return rec, fmt.Errorf("renewing lease on %s: %w", spec.ID, err)
	}

	o.logger.Info("running step", "cluster", spec.ID, "step", step.Name(), "phase", rec.Phase)

	if err := step.Run(ctx, spec, rec); err != nil {
		// The phase is deliberately not advanced: the next run resumes here and
		// re-executes this step.
		return rec, fmt.Errorf("%s: %w", step.Name(), err)
	}

	updated, err := o.registry.UpdatePhase(ctx, rec, next)
	if err != nil {
		return rec, fmt.Errorf("recording phase %s for %s: %w", next, spec.ID, err)
	}

	o.logger.Info("phase complete", "cluster", spec.ID, "phase", next)
	return updated, nil
}

// release drops the lease, logging rather than failing: the run's outcome is
// already decided, and the lease expires on its own regardless.
//
// It detaches from the caller's cancellation, so an interrupted run still frees
// the cluster instead of leaving it blocked until the TTL elapses — which is
// the common case when someone hits Ctrl-C on a long provisioning run.
func (o *Orchestrator) release(ctx context.Context, id core.ClusterID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()

	if err := o.registry.ReleaseLease(ctx, id, o.holder); err != nil {
		o.logger.Warn("could not release lease; it will expire on its own",
			"cluster", id, "holder", o.holder, "error", err)
	}
}
