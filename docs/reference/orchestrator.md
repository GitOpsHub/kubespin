# internal/orchestrator

`internal/orchestrator` is the piece that turns the phase state machine into an
actual run. It drives a single cluster through `pending → cluster-created →
identity-bound → repo-pushed → argocd-installed → ready` for `apply`, and the
reverse teardown `decommissioning → decommissioned` for `delete`, recording
the phase in the Fleet Registry after each step so a failed run resumes at its
last completed phase instead of restarting from scratch.

`apply`'s idempotence and split-diff routing live in two different places in
this package. Getting a cluster to `ready` the first time is the phase walk in
[orchestrator.go](https://github.com/GitOpsHub/kubespin/blob/main/internal/orchestrator/orchestrator.go):
each phase step runs exactly once and is only recorded as done once it
succeeds, so re-running `apply` against a part-way cluster re-enters at the
recorded phase rather than repeating finished work. Keeping a `ready` cluster
converged on every subsequent `apply` is the job of `ReadyReconcile` in
[steps.go](https://github.com/GitOpsHub/kubespin/blob/main/internal/orchestrator/steps.go):
it resolves the cluster's profile, hashes desired state, and routes the diff —
an infra change becomes a call to `cloud.Cluster.Reconcile` (a cloud SDK call),
an addon change becomes `repo.ReconcileAddons` (a git commit + push against
`.state.yaml`) — so a no-change `apply` makes neither call. Each orchestrator
step maps to exactly one registry phase transition: `Orchestrator.advance`
runs the `Step` registered for the cluster's current phase, and only calls
`registry.UpdatePhase` to the next phase if that step returned no error.

## Step order

### Apply (`ProvisioningSteps`, steps.go)

| Phase (from) | Step | What it does | Packages called |
|---|---|---|---|
| `pending` | `createClusterStep` | Resolves the network (`cloud.Network.EnsureNetwork`, skipped if `cloud.Network` is nil), requests the cluster (`cloud.Cluster.Create`), waits for the control plane (`provisioner.WaitUntilActive`), reconciles node pools (`cloud.Cluster.Reconcile` — this is what actually attaches them on a first run), then opens egress to the ingestion endpoint (`openEgress` → `cloud.Network.AllowEgress`). | `internal/provisioner` |
| `cluster-created` | `bindIdentityStep` | Provisions the status reporter's workload identity (`cloud.Identity.ProvisionForComponent`), then describes the cluster (`cloud.Cluster.Describe`) to capture its OIDC issuer and records it via `reg.RecordOIDCIssuer` — the issuer the Central Ingestion API later verifies the reporter's signature against. | `internal/provisioner`, `internal/registry` |
| `identity-bound` | `seedRepoStep` | Resolves the cluster's profile (`resolveProfile`: catalog resolve → provider template → override merge → ingress/access-mode templating) and creates/seeds the cluster's repository with its initial `cluster.yaml`, `addons.yaml`, `.state.yaml` (`repo.Seed`). | `internal/catalog`, `internal/repo` |
| `repo-pushed` | `installArgoCDStep` | Builds a `*rest.Config` for the cluster via `provisioner.RESTConfigProvisioner`, resolves the profile, installs Argo CD (`installer.Install`), applies a repo-credentials Secret and the self-referential root Application directly to the cluster (`applier.Apply` — never committed to the repo it manages), then commits the app-of-apps addon Applications (`repo.ReconcileAppOfApps`). | `internal/argocd`, `internal/catalog`, `internal/repo`, `internal/provisioner` |
| `argocd-installed` | (default no-op: `DefaultSteps()["argocd-installed"] = "verify addons healthy"`) | Placeholder; `ProvisioningSteps` does not override this phase. | — |

Once the walk lands the cluster at `ready`, `Orchestrator.Apply` invokes the
`ReadyReconcile` function (if configured) even on runs that find the cluster
already `ready` — this is the ongoing split-diff reconciliation described
above, not a phase step.

### Delete (`Teardown`, steps.go + delete.go)

`Orchestrator.Delete` marks the registry `decommissioning`, runs the supplied
`TeardownFunc`, then marks it `decommissioned`. `Teardown` (steps.go) builds
that function, deliberately in the reverse order of `apply` (identity, then
cluster, then repo) so nothing is deleted while a later step might still need
it:

1. **Deprovision identity** — `cloud.Identity.Deprovision` for the status
   reporter component (`internal/provisioner`).
2. **Delete cluster** — `cloud.Cluster.Delete`, then blocks on
   `provisioner.WaitUntilGone` so the phase is only recorded once the cloud
   confirms the cluster is actually gone (node pools drain first; this can
   take several minutes).
3. **Archive repository** — `repoProv.Archive` (`internal/repo`) — the repo is
   archived, never deleted, per the cluster-repo contract.

Every sub-step is idempotent (deprovisioning/deleting/archiving something
already gone converges rather than erroring), so a retried `delete` re-runs
`Teardown` from the top rather than needing to track which sub-step it
reached.

## Exported types

### `Step`

```go
type Step interface {
	Name() string
	Run(ctx context.Context, spec core.ClusterSpec, rec registry.Record) error
}
```

Performs the work that moves a cluster out of one phase. A step must be safe
to re-run: a resumed apply re-executes the step for the phase it stopped at,
because the phase is only recorded once the step succeeded.

### `StepFunc`

```go
type StepFunc struct {
	Label string
	Fn    func(ctx context.Context, spec core.ClusterSpec, rec registry.Record) error
}
```

Adapts a plain function to `Step`. `Name()` returns `Label`; `Run` calls `Fn`
if set, otherwise no-ops (this is what makes the `DefaultSteps()` placeholders
work — they carry a `Label` and no `Fn`).

### `ReconcileFunc`

```go
type ReconcileFunc func(ctx context.Context, spec core.ClusterSpec, rec registry.Record) error
```

Reconciles a cluster that is already at `PhaseReady`. Built by `ReadyReconcile`
and installed via `WithReadyReconcile`.

### `TeardownFunc` (delete.go)

```go
type TeardownFunc func(ctx context.Context, spec core.ClusterSpec, rec registry.Record) error
```

Performs the actual cleanup for a decommissioning cluster. Runs once the
cluster is recorded `PhaseDecommissioning`, so a crashed teardown resumes as a
teardown on retry rather than an ordinary `apply`.

### `Cloud`

```go
type Cloud struct {
	Cluster  provisioner.ClusterProvisioner
	Identity provisioner.IdentityProvisioner
	Network  provisioner.NetworkProvisioner

	IngestionEndpoint provisioner.EgressDestination
	Wait provisioner.WaitOptions
}
```

Bundles the provisioners for one cloud so per-cloud construction lives in one
place — adding GCP and Azure is a matter of building this struct differently
rather than changing the orchestrator. `IngestionEndpoint` is the Central
Ingestion API the status reporter pushes to, the only destination a cluster's
egress must permit; if its `Host` is empty, `openEgress` logs a warning and
allows nothing. `Wait` tunes how cluster creation/deletion is polled.

### `Orchestrator`

```go
type Orchestrator struct {
	registry       registry.Registry
	steps          map[core.Phase]Step
	holder         string
	leaseTTL       time.Duration
	now            func() time.Time
	logger         *slog.Logger
	readyReconcile ReconcileFunc
}
```

Drives one cluster's provisioning at a time. Built with `New(reg, opts...)`
and configured through the `Option` functions below. Invariants:

- `holder` must be unique per run — two runs sharing a holder would each
  believe they own the lease (see `defaultHolder`, which combines hostname and
  PID).
- Exactly one `Orchestrator.Apply` or `Orchestrator.Delete` call may hold a
  given cluster's lease at a time; a second concurrent call gets `ErrBusy`.

### `Option`

```go
type Option func(*Orchestrator)
```

Functional option for `New`. Provided options: `WithSteps`, `WithHolder`,
`WithLeaseTTL`, `WithClock`, `WithLogger`, `WithReadyReconcile`.

## Exported errors and constants

```go
var ErrBusy = errors.New("cluster is being provisioned by another run")
var ErrDecommissioning = errors.New("cluster is decommissioning or decommissioned")
var ErrLeaseLost = errors.New("lost the cluster lease mid-run")

const DefaultLeaseTTL = 15 * time.Minute
```

`ErrBusy` — another run holds the cluster's lease (`AcquireLease` returned
`registry.ErrLeaseHeld`). `ErrDecommissioning` — `Apply` was called on a
cluster whose phase is `decommissioning` or `decommissioned`; reviving one is
not a phase transition, it is a new cluster. `ErrLeaseLost` — a run can no
longer prove it holds the lease (see `keepLeaseAlive`); deliberately fatal,
since another `apply` may already be provisioning the same cluster.
`DefaultLeaseTTL` bounds how long a crashed run can block a cluster; the
orchestrator renews it before each step and on a timer during long steps, so
this only has to outlast the longest single step's renewal failures, not the
whole run.

## Exported functions

### `DefaultSteps`

```go
func DefaultSteps() map[core.Phase]Step
```

Returns a no-op `Step` (label only, no `Fn`) for every phase from `pending`
through `argocd-installed`. Placeholders until real steps are wired in by
`ProvisioningSteps`; their labels document what each phase is waiting on.

### `WithSteps`, `WithHolder`, `WithLeaseTTL`, `WithClock`, `WithLogger`, `WithReadyReconcile`

```go
func WithSteps(steps map[core.Phase]Step) Option
func WithHolder(holder string) Option
func WithLeaseTTL(ttl time.Duration) Option
func WithClock(now func() time.Time) Option
func WithLogger(logger *slog.Logger) Option
func WithReadyReconcile(fn ReconcileFunc) Option
```

Standard functional options replacing, respectively: the phase→step map, the
lease holder identity, the lease TTL, the time source, the logger, and the
function `Apply` runs whenever it leaves a cluster at `PhaseReady` (whether
that's the outcome of this call's phase walk, or the cluster was already ready
when `Apply` was called).

### `New`

```go
func New(reg registry.Registry, opts ...Option) *Orchestrator
```

Builds an `Orchestrator` over `reg` with `DefaultSteps()`, a hostname/PID
holder, `DefaultLeaseTTL`, `time.Now`, and `slog.Default()`, then applies
`opts`.

### `(*Orchestrator) Apply`

```go
func (o *Orchestrator) Apply(ctx context.Context, spec core.ClusterSpec) (registry.Record, error)
```

Validates `spec`, ensures a registry record exists (`ensureRecord`, creating
one at `pending` if new), acquires the cluster's lease, re-reads the record
under the lease (another run may have advanced it between the first read and
acquiring), then walks the phase state machine to `ready` via `run`. If the
resulting phase is `ready` and `WithReadyReconcile` was set, calls that
function. Returns the final `registry.Record` and, on lease loss, wraps the
error with `ErrLeaseLost` context. Idempotent: a cluster already at `ready`
runs no steps and writes nothing.

### `(*Orchestrator) Delete`

```go
func (o *Orchestrator) Delete(ctx context.Context, spec core.ClusterSpec, teardown TeardownFunc) (registry.Record, error)
```

Reads the cluster's record; no-ops if already `decommissioned`. Otherwise
acquires the lease, re-reads under it, marks `PhaseDecommissioning` if not
already there, runs `teardown`, then marks `PhaseDecommissioned`. On teardown
failure the phase is deliberately left at `decommissioning` so a retried
`delete` resumes teardown rather than believing the cluster is still live.

### `ProvisioningSteps`

```go
func ProvisioningSteps(
	cloud Cloud, repoProv repo.Provisioner, resolver catalog.Resolver, reg registry.Registry,
	installer argocd.Installer, applier argocd.KubeApplier, logger *slog.Logger,
) map[core.Phase]Step
```

Builds the real `pending`→`argocd-installed` steps described in the Apply
table above, overriding `DefaultSteps()`.

### `ReadyReconcile`

```go
func ReadyReconcile(cloud Cloud, repoProv repo.Provisioner, resolver catalog.Resolver, logger *slog.Logger) ReconcileFunc
```

Builds the `ReconcileFunc` that keeps a `ready` cluster converged on every
subsequent `apply`: `cloud.Cluster.Reconcile` for infra drift,
`repo.ReconcileAddons` (after `resolveProfile`) for addon drift. A no-change
run makes neither call — this is where `apply`'s split-diff idempotence for
already-provisioned clusters lives.

### `Teardown`

```go
func Teardown(cloud Cloud, repoProv repo.Provisioner, logger *slog.Logger) TeardownFunc
```

Builds the `TeardownFunc` performing the reverse-teardown sequence described
in the Delete table above: identity deprovision → cluster delete (blocking on
`provisioner.WaitUntilGone`) → repository archive.

## Internal machinery worth knowing

- **`advance`** (orchestrator.go) runs the `Step` for the record's current
  phase, renewing the lease first as a fast-fail check, and only calls
  `registry.UpdatePhase` to `next` if the step succeeds — a failed step leaves
  the phase unchanged so a resumed run re-executes it.
- **`keepLeaseAlive`** (lease.go) renews the lease on a timer (every
  `leaseTTL / 3`) in a background goroutine, because a single step (e.g.
  waiting for a control plane) can run far longer than the lease TTL.
  Renewing only between steps would let the lease lapse mid-step and let a
  second `apply` provision the same cluster concurrently. It returns a
  derived `context.Context` that is cancelled with `ErrLeaseLost` the moment
  renewal can no longer prove the lease is held (either an unambiguous
  `registry.ErrLeaseLost`/`registry.ErrNotFound`, or transient renewal
  failures persisting past the lease's last known expiry).
- **`leaseFailure`** (lease.go) rewrites an error as `ErrLeaseLost` context
  when the run context was cancelled by `keepLeaseAlive`, so a lost lease
  reads as "another run took over" instead of an unexplained context
  cancellation.
