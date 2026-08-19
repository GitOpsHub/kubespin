# internal/orchestrator

`internal/orchestrator` is the piece that turns the phase state machine into an
actual run. It drives a single cluster through `pending → cluster-created →
identity-bound → repo-pushed → argocd-installed → ready` for `apply`, and the
reverse teardown `decommissioning → decommissioned` for `delete`, recording
the phase in the Fleet Registry after each step so a failed run resumes at its
last completed phase instead of restarting from scratch.

## Quick reference

| Name | Kind | File | Summary |
|---|---|---|---|
| [`Step`](#step) | interface | orchestrator.go | Work that moves a cluster out of one phase; must be safe to re-run. |
| [`StepFunc`](#stepfunc) | type | orchestrator.go | Adapts a plain function to `Step`. |
| [`ReconcileFunc`](#reconcilefunc) | type | orchestrator.go | Reconciles a cluster already at `PhaseReady`. |
| [`Orchestrator`](#orchestrator) | type | orchestrator.go | Drives one cluster's provisioning at a time; holds the phase→step map and lease config. |
| [`Option`](#option) | type | orchestrator.go | Functional option for `New`. |
| [`ErrBusy`](#errbusy) | var | orchestrator.go | Another run holds the cluster's lease. |
| [`ErrDecommissioning`](#errbusy) | var | orchestrator.go | `Apply` called on a decommissioning/decommissioned cluster. |
| [`DefaultLeaseTTL`](#errbusy) | const | orchestrator.go | Default lease TTL (15 minutes). |
| [`DefaultSteps`](#defaultsteps) | func | orchestrator.go | No-op `Step` placeholders for every phase through `argocd-installed`. |
| [`WithSteps`, `WithHolder`, `WithLeaseTTL`, `WithClock`, `WithLogger`, `WithReadyReconcile`](#withsteps-withholder-withleasettl-withclock-withlogger-withreadyreconcile) | func | orchestrator.go | Functional options for `New`. |
| [`New`](#new) | func | orchestrator.go | Builds an `Orchestrator` with defaults plus `opts`. |
| [`(*Orchestrator) Apply`](#orchestrator-apply) | func | orchestrator.go | Walks a cluster's phase state machine to `ready`. |
| [`Cloud`](#cloud) | type | steps.go | Bundles the provisioners for one cloud. |
| [`ProvisioningSteps`](#provisioningsteps) | func | steps.go | Builds the real `pending`→`argocd-installed` steps. |
| [`ReadyReconcile`](#readyreconcile) | func | steps.go | Builds the `ReconcileFunc` that keeps a `ready` cluster converged. |
| [`Teardown`](#teardown) | func | steps.go | Builds the `TeardownFunc` for reverse teardown. |
| [`TeardownFunc`](#teardownfunc) | type | delete.go | Performs the actual cleanup for a decommissioning cluster. |
| [`(*Orchestrator) Delete`](#orchestrator-delete) | func | delete.go | Marks decommissioning, runs teardown, marks decommissioned. |
| [`ErrLeaseLost`](#errleaselost) | var | lease.go | A run can no longer prove it holds the lease; deliberately fatal. |
| [`keepLeaseAlive`](#keepleasealive) | func (internal) | lease.go | Renews the lease on a timer in a background goroutine. |
| [`leaseFailure`](#leasefailure) | func (internal) | lease.go | Rewrites a cancellation error as `ErrLeaseLost` context. |

## Step order

### Apply (`ProvisioningSteps`, steps.go)

| Step | Registry phase (from) | Calls into |
|---|---|---|
| `createClusterStep` | `pending` | `internal/provisioner` — resolves the network (`cloud.Network.EnsureNetwork`, skipped if `cloud.Network` is nil), requests the cluster (`cloud.Cluster.Create`), waits for the control plane (`provisioner.WaitUntilActive`), reconciles node pools (`cloud.Cluster.Reconcile` — this is what actually attaches them on a first run), then opens egress to the ingestion endpoint (`openEgress` → `cloud.Network.AllowEgress`). |
| `bindIdentityStep` | `cluster-created` | `internal/provisioner`, `internal/registry` — provisions the status reporter's workload identity (`cloud.Identity.ProvisionForComponent`), then describes the cluster (`cloud.Cluster.Describe`) to capture its OIDC issuer and records it via `reg.RecordOIDCIssuer` — the issuer the Central Ingestion API later verifies the reporter's signature against. |
| `seedRepoStep` | `identity-bound` | `internal/catalog`, `internal/repo` — resolves the cluster's profile (`catalog.ResolveForCluster`: catalog resolve → provider template → argocd stand-in → override merge → ingress/access-mode templating) and creates/seeds the cluster's repository with its initial `cluster.yaml`, `addons.yaml`, `.state.yaml` (`repo.Seed`). |
| `installArgoCDStep` | `repo-pushed` | `internal/argocd`, `internal/catalog`, `internal/repo`, `internal/provisioner` — builds a `*rest.Config` for the cluster via `provisioner.RESTConfigProvisioner`, resolves the profile (`catalog.ResolveForCluster`), looks up its `"argocd"` addon (`Profile.Addon`, always present) and installs it (`installer.Install`), applies a repo-credentials Secret and the self-referential root Application directly to the cluster (`applier.Apply` — never committed to the repo it manages), then commits the app-of-apps addon Applications (`repo.ReconcileAppOfApps`). |
| (default no-op: `DefaultSteps()["argocd-installed"] = "verify addons healthy"`) | `argocd-installed` | — Placeholder; `ProvisioningSteps` does not override this phase. |

Once the walk lands the cluster at `ready`, `Orchestrator.Apply` invokes the
`ReadyReconcile` function (if configured) even on runs that find the cluster
already `ready` — this is the ongoing split-diff reconciliation described
below, not a phase step.

### Delete (`Teardown`, steps.go + delete.go)

`Orchestrator.Delete` marks the registry `decommissioning`, runs the supplied
`TeardownFunc`, then marks it `decommissioned`. `Teardown` (steps.go) builds
that function, deliberately in the reverse order of `apply` (identity, load
balancers, cluster, network, then repo) so nothing is deleted while a later
step might still need it:

| Step | Registry phase | Calls into |
|---|---|---|
| Deprovision identity | `decommissioning` | `internal/provisioner` — `cloud.Identity.Deprovision` for the status reporter component. |
| Drain load balancers | `decommissioning` | `drainLoadBalancers` (steps.go) — builds a `k8s.io/client-go/kubernetes` clientset from `restConfigFor` and deletes every `Service` of type `LoadBalancer` across all namespaces, waiting (bounded, `drainLoadBalancersTimeout`) for each to actually disappear before returning. A cluster that cannot be reached (already gone from an earlier interrupted teardown, or never became active) is a no-op, not a failure — there is nothing to drain. Exists because deleting the cluster does not clean up the cloud load balancer a `Service type=LoadBalancer` (e.g. Argo CD's own exposure) owns; without this it survives the cluster, billing indefinitely, and blocks the network-delete step below with a dependency violation. |
| Delete cluster | `decommissioning` | `internal/provisioner` — `cloud.Cluster.Delete`, then blocks on `provisioner.WaitUntilGone` so the phase is only recorded once the cloud confirms the cluster is actually gone (node pools drain first; this can take several minutes). |
| Delete network | `decommissioning` | `internal/provisioner` — `cloud.Network.DeleteNetwork`, reversing `EnsureNetwork`. Identifies what to delete by the same deterministic name `EnsureNetwork` used, not by `spec.Subnets`, so it is safe even when `delete` was not given the same `--subnets` an earlier `apply` was; an operator-supplied network (or one already gone) is a no-op. |
| Archive repository | `decommissioning` → `decommissioned` | `internal/repo` — `repoProv.Archive`; the repo is archived, never deleted, per the cluster-repo contract. |

Every sub-step is idempotent (deprovisioning/deleting/archiving something
already gone converges rather than erroring), so a retried `delete` re-runs
`Teardown` from the top rather than needing to track which sub-step it
reached.

## orchestrator.go

`apply`'s idempotence and split-diff routing live in two different places in
this package. Getting a cluster to `ready` the first time is the phase walk in
[orchestrator.go](https://github.com/GitOpsHub/kubespin/blob/main/internal/orchestrator/orchestrator.go):
each phase step runs exactly once and is only recorded as done once it
succeeds, so re-running `apply` against a part-way cluster re-enters at the
recorded phase rather than repeating finished work. Each orchestrator step
maps to exactly one registry phase transition: `Orchestrator.advance` runs the
`Step` registered for the cluster's current phase, and only calls
`registry.UpdatePhase` to the next phase if that step returned no error.

#### `Step`

??? abstract "`Step` — interface"

    ```go
    type Step interface {
    	Name() string
    	Run(ctx context.Context, spec core.ClusterSpec, rec registry.Record) error
    }
    ```

    - **Behavior**: performs the work that moves a cluster out of one phase.
    - **Invariant**: a step must be safe to re-run — a resumed apply
      re-executes the step for the phase it stopped at, because the phase is
      only recorded once the step succeeded.

#### `StepFunc`

??? abstract "`StepFunc` — type"

    ```go
    type StepFunc struct {
    	Label string
    	Fn    func(ctx context.Context, spec core.ClusterSpec, rec registry.Record) error
    }
    ```

    - **Behavior**: adapts a plain function to `Step`. `Name()` returns
      `Label`; `Run` calls `Fn` if set, otherwise no-ops.
    - **Used by**: `DefaultSteps()` placeholders, which carry a `Label` and no
      `Fn`.

#### `ReconcileFunc`

??? abstract "`ReconcileFunc` — type"

    ```go
    type ReconcileFunc func(ctx context.Context, spec core.ClusterSpec, rec registry.Record) error
    ```

    - **Behavior**: reconciles a cluster that is already at `PhaseReady`.
    - **Built by**: `ReadyReconcile`, installed via `WithReadyReconcile`.

#### `Cloud`

??? abstract "`Cloud` — type (bundling struct, defined in steps.go)"

    See [`Cloud`](#cloud) under steps.go.

#### `Orchestrator`

??? abstract "`Orchestrator` — type"

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

    - **Behavior**: drives one cluster's provisioning at a time.
    - **Built by**: `New(reg, opts...)` and configured through the `Option`
      functions.
    - **Invariants**:
        - `holder` must be unique per run — two runs sharing a holder would
          each believe they own the lease (see `defaultHolder`, which
          combines hostname and PID).
        - Exactly one `Orchestrator.Apply` or `Orchestrator.Delete` call may
          hold a given cluster's lease at a time; a second concurrent call
          gets `ErrBusy`.

#### `Option`

??? abstract "`Option` — type"

    ```go
    type Option func(*Orchestrator)
    ```

    - **Behavior**: functional option for `New`.
    - **Provided options**: `WithSteps`, `WithHolder`, `WithLeaseTTL`,
      `WithClock`, `WithLogger`, `WithReadyReconcile`.

#### `ErrBusy`

??? note "`ErrBusy`, `ErrDecommissioning`, `DefaultLeaseTTL` — vars/const"

    ```go
    var ErrBusy = errors.New("cluster is being provisioned by another run")
    var ErrDecommissioning = errors.New("cluster is decommissioning or decommissioned")

    const DefaultLeaseTTL = 15 * time.Minute
    ```

    - **`ErrBusy`**: another run holds the cluster's lease (`AcquireLease`
      returned `registry.ErrLeaseHeld`).
    - **`ErrDecommissioning`**: `Apply` was called on a cluster whose phase is
      `decommissioning` or `decommissioned`; reviving one is not a phase
      transition, it is a new cluster.
    - **`DefaultLeaseTTL`**: bounds how long a crashed run can block a
      cluster; the orchestrator renews it before each step and on a timer
      during long steps, so this only has to outlast the longest single
      step's renewal failures, not the whole run.

    See also [`ErrLeaseLost`](#errleaselost) (lease.go), which rounds out the
    package's exported error set.

#### `DefaultSteps`

??? note "`DefaultSteps` — func"

    ```go
    func DefaultSteps() map[core.Phase]Step
    ```

    - **Returns**: a no-op `Step` (label only, no `Fn`) for every phase from
      `pending` through `argocd-installed`.
    - **Behavior**: placeholders until real steps are wired in by
      `ProvisioningSteps`; their labels document what each phase is waiting
      on.

#### `WithSteps`, `WithHolder`, `WithLeaseTTL`, `WithClock`, `WithLogger`, `WithReadyReconcile`

??? note "`WithSteps`, `WithHolder`, `WithLeaseTTL`, `WithClock`, `WithLogger`, `WithReadyReconcile` — funcs"

    ```go
    func WithSteps(steps map[core.Phase]Step) Option
    func WithHolder(holder string) Option
    func WithLeaseTTL(ttl time.Duration) Option
    func WithClock(now func() time.Time) Option
    func WithLogger(logger *slog.Logger) Option
    func WithReadyReconcile(fn ReconcileFunc) Option
    ```

    - **Behavior**: standard functional options replacing, respectively: the
      phase→step map, the lease holder identity, the lease TTL, the time
      source, the logger, and the function `Apply` runs whenever it leaves a
      cluster at `PhaseReady` (whether that's the outcome of this call's
      phase walk, or the cluster was already ready when `Apply` was called).

#### `New`

??? note "`New` — func"

    ```go
    func New(reg registry.Registry, opts ...Option) *Orchestrator
    ```

    - **Behavior**: builds an `Orchestrator` over `reg` with `DefaultSteps()`,
      a hostname/PID holder, `DefaultLeaseTTL`, `time.Now`, and
      `slog.Default()`, then applies `opts`.

#### `(*Orchestrator) Apply`

??? note "`(*Orchestrator) Apply` — func"

    ```go
    func (o *Orchestrator) Apply(ctx context.Context, spec core.ClusterSpec) (registry.Record, error)
    ```

    - **Behavior**: validates `spec`, ensures a registry record exists
      (`ensureRecord`, creating one at `pending` if new), acquires the
      cluster's lease, re-reads the record under the lease (another run may
      have advanced it between the first read and acquiring), then walks the
      phase state machine to `ready` via `run`.
    - **Behavior**: if the resulting phase is `ready` and
      `WithReadyReconcile` was set, calls that function.
    - **Returns**: the final `registry.Record` and, on lease loss, wraps the
      error with `ErrLeaseLost` context.
    - **Invariant**: idempotent — a cluster already at `ready` runs no steps
      and writes nothing.

??? note "`advance` — internal func"

    `advance` (orchestrator.go) runs the `Step` for the record's current
    phase, renewing the lease first as a fast-fail check, and only calls
    `registry.UpdatePhase` to `next` if the step succeeds — a failed step
    leaves the phase unchanged so a resumed run re-executes it.

## steps.go

Keeping a `ready` cluster converged on every subsequent `apply` is the job of
`ReadyReconcile` in
[steps.go](https://github.com/GitOpsHub/kubespin/blob/main/internal/orchestrator/steps.go):
it resolves the cluster's profile, hashes desired state, and routes the diff
— an infra change becomes a call to `cloud.Cluster.Reconcile` (a cloud SDK
call), an addon change becomes `repo.ReconcileAddons` (a git commit + push
against `.state.yaml`) — so a no-change `apply` makes neither call.

??? abstract "`Cloud` — type"

    ```go
    type Cloud struct {
    	Cluster  provisioner.ClusterProvisioner
    	Identity provisioner.IdentityProvisioner
    	Network  provisioner.NetworkProvisioner

    	IngestionEndpoint provisioner.EgressDestination
    	Wait provisioner.WaitOptions
    }
    ```

    - **Behavior**: bundles the provisioners for one cloud so per-cloud
      construction lives in one place — adding GCP and Azure is a matter of
      building this struct differently rather than changing the
      orchestrator.
    - **`IngestionEndpoint`**: the Central Ingestion API the status reporter
      pushes to, the only destination a cluster's egress must permit; if its
      `Host` is empty, `openEgress` logs a warning and allows nothing.
    - **`Wait`**: tunes how cluster creation/deletion is polled.

#### `ProvisioningSteps`

??? note "`ProvisioningSteps` — func"

    ```go
    func ProvisioningSteps(
    	cloud Cloud, repoProv repo.Provisioner, resolver catalog.Resolver, reg registry.Registry,
    	installer argocd.Installer, applier argocd.KubeApplier, logger *slog.Logger,
    ) map[core.Phase]Step
    ```

    - **Behavior**: builds the real `pending`→`argocd-installed` steps
      described in the [Apply step order table](#apply-provisioningsteps-stepsgo),
      overriding `DefaultSteps()`.

#### `ReadyReconcile`

??? note "`ReadyReconcile` — func"

    ```go
    func ReadyReconcile(cloud Cloud, repoProv repo.Provisioner, resolver catalog.Resolver, logger *slog.Logger) ReconcileFunc
    ```

    - **Behavior**: builds the `ReconcileFunc` that keeps a `ready` cluster
      converged on every subsequent `apply`: `cloud.Cluster.Reconcile` for
      infra drift, `repo.ReconcileAddons` (after `catalog.ResolveForCluster`)
      for addon drift.
    - **Invariant**: a no-change run makes neither call — this is where
      `apply`'s split-diff idempotence for already-provisioned clusters
      lives.

#### `Teardown`

??? note "`Teardown` — func"

    ```go
    func Teardown(cloud Cloud, repoProv repo.Provisioner, logger *slog.Logger) TeardownFunc
    ```

    - **Behavior**: builds the `TeardownFunc` performing the reverse-teardown
      sequence described in the
      [Delete step order table](#delete-teardown-stepsgo-deletego):
      identity deprovision → cluster delete (blocking on
      `provisioner.WaitUntilGone`) → repository archive.

??? note "Step functions — `createClusterStep`, `bindIdentityStep`, `seedRepoStep`, `installArgoCDStep`, `openEgress` — internal funcs"

    - **Behavior**: unexported implementations backing the phase steps
      described in the [Apply step order table](#apply-provisioningsteps-stepsgo)
      above. Profile resolution itself — catalog resolve → provider template
      → argocd stand-in → override merge → ingress/access-mode templating —
      no longer lives here: `seedRepoStep`, `installArgoCDStep`, and
      `ReadyReconcile` all call `catalog.ResolveForCluster`
      (`internal/catalog/resolve.go`), the same seam `internal/fleet.UpdateOne`
      uses for `fleet update`, so the two commands can never resolve a given
      cluster's profile differently.

## delete.go

`Orchestrator.Delete` marks the registry `decommissioning`, runs the supplied
`TeardownFunc`, then marks it `decommissioned` — see the
[Delete step order table](#delete-teardown-stepsgo-deletego) for what the
teardown itself does.

#### `TeardownFunc`

??? abstract "`TeardownFunc` — type"

    ```go
    type TeardownFunc func(ctx context.Context, spec core.ClusterSpec, rec registry.Record) error
    ```

    - **Behavior**: performs the actual cleanup for a decommissioning
      cluster.
    - **Invariant**: runs once the cluster is recorded
      `PhaseDecommissioning`, so a crashed teardown resumes as a teardown on
      retry rather than an ordinary `apply`.

#### `(*Orchestrator) Delete`

??? note "`(*Orchestrator) Delete` — func"

    ```go
    func (o *Orchestrator) Delete(ctx context.Context, spec core.ClusterSpec, teardown TeardownFunc) (registry.Record, error)
    ```

    - **Behavior**: reads the cluster's record; no-ops if already
      `decommissioned`. Otherwise acquires the lease, re-reads under it,
      marks `PhaseDecommissioning` if not already there, runs `teardown`,
      then marks `PhaseDecommissioned`.
    - **Invariant**: on teardown failure the phase is deliberately left at
      `decommissioning` so a retried `delete` resumes teardown rather than
      believing the cluster is still live.

## lease.go

`keepLeaseAlive` renews the lease on a timer in a background goroutine,
because a single step (e.g. waiting for a control plane) can run far longer
than the lease TTL. Renewing only between steps would let the lease lapse
mid-step and let a second `apply` provision the same cluster concurrently.

#### `ErrLeaseLost`

??? note "`ErrLeaseLost` — var"

    ```go
    var ErrLeaseLost = errors.New("lost the cluster lease mid-run")
    ```

    - **Behavior**: a run can no longer prove it holds the lease (see
      `keepLeaseAlive`); deliberately fatal, since another `apply` may
      already be provisioning the same cluster.

#### `keepLeaseAlive`

??? note "`keepLeaseAlive` — internal func"

    ```go
    func (o *Orchestrator) keepLeaseAlive(ctx context.Context, id core.ClusterID) (context.Context, func())
    ```

    - **Behavior**: renews the lease on a timer (every `leaseTTL / 3`) in a
      background goroutine.
    - **Returns**: a derived `context.Context` that is cancelled with
      `ErrLeaseLost` the moment renewal can no longer prove the lease is
      held (either an unambiguous `registry.ErrLeaseLost`/
      `registry.ErrNotFound`, or transient renewal failures persisting past
      the lease's last known expiry).

#### `leaseFailure`

??? note "`leaseFailure` — internal func"

    ```go
    func leaseFailure(runCtx context.Context, err error) error
    ```

    - **Behavior**: rewrites an error as `ErrLeaseLost` context when the run
      context was cancelled by `keepLeaseAlive`, so a lost lease reads as
      "another run took over" instead of an unexplained context
      cancellation.
