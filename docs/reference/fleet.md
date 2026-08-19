# internal/fleet

`internal/fleet` implements the fleet-wide operations — `audit`, `update`, and `status` (plus the `dashboard` view) — as declared in the package doc comment in [`fleet.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/fleet.go). Unlike `internal/orchestrator`, which drives one cluster at a time through `apply`/`delete`, everything here starts from a `registry.Registry.List` call and fans out across every matching cluster. `Audit` is read-only and diffs live cloud state against each cluster's `cluster.yaml`; `Update` is the one write path, patching a component's version into addon overrides and committing `addons.yaml`; `Status` and `Dashboard` read only from the Fleet Registry and never connect to a cluster, so a `fleet status` on an unreachable cluster cannot hang — the cluster is flagged *stale* instead.

## Quick reference

| Name | Kind | File | Summary |
|---|---|---|---|
| [`Option` / `options`](#option-options) | type | fleet.go | Configures a fleet-wide operation (logger, clock) |
| [`ClusterProvisionerFactory`](#clusterprovisionerfactory) | type | fleet.go | Builds a `ClusterProvisioner` for a cluster's provider/region |
| [`AuditResult`](#auditresult) | type | fleet.go | One cluster's audit outcome (findings or error) |
| [`Audit`](#audit) | func | fleet.go | Fans out `AuditOne` across all matching clusters, records findings |
| [`Finding`](#finding) | type | audit.go | One drift a cluster's live infra shows against `cluster.yaml` |
| [`AuditOne`](#auditone) | func | audit.go | Diffs one cluster's live state against desired `cluster.yaml` |
| [`desiredSpec`](#desiredspec) | func | audit.go | Clones a cluster's repo and parses `cluster.yaml` |
| [`ClusterStatus`](#clusterstatus) | type | status.go | One cluster's row in a `fleet status` report |
| [`Status`](#status) | func | status.go | Builds `ClusterStatus` rows from registry data alone |
| [`UpdateResult`](#updateresult) | type | update.go | One cluster's outcome from an update wave |
| [`UpdateOne`](#updateone) | func | update.go | Pins a component's version into one cluster's overrides and commits |
| [`setComponentVersion`](#setcomponentversion) | func | update.go | Returns overrides with a component's version pinned |
| [`Update`](#update) | func | update.go | Rolls a version out fleet-wide, canary wave first |
| [`countFailedUpdates`](#countfailedupdates) | func | update.go | Counts results with `Err != nil` |
| [`updateWave`](#updatewave) | func | update.go | Runs `updateRecord` across a wave on a worker pool |
| [`updateRecord`](#updaterecord) | func | update.go | Updates one cluster's addon override and commits |
| [`DashboardRow`](#dashboardrow) | type | dashboard.go | One cluster's row in `fleet dashboard`, with full findings |
| [`Dashboard`](#dashboard) | func | dashboard.go | Builds `DashboardRow`s from registry data alone |

## fleet.go

#### `Option` / `options`

??? abstract "`Option` / `options` — configures a fleet-wide operation"

    ```go
    type Option func(*options)

    type options struct {
        logger *slog.Logger
        now    func() time.Time
    }
    ```

    Defined in [`fleet.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/fleet.go).

    - **Purpose**: `Option` configures a fleet-wide operation; it is variadic on `Status`, `Audit`, and `Update` so those functions keep their existing positional signatures.
    - **Constructors**:
        - `WithLogger(logger *slog.Logger) Option` — sets the logger. A nil logger is ignored (falls back to `slog.Default()`). Fleet logging is supplementary diagnostic detail for operators running with `--log-level debug`; `internal/cli` does its own user-facing reporting, so nothing logged here duplicates that.
        - `WithClock(now func() time.Time) Option` — replaces `Audit`'s time source, so a finding's recorded timestamp is testable without depending on wall-clock time. A nil func is ignored.
    - **`resolveOptions(opts []Option) options`** (unexported): applies defaults (`slog.Default()`, `time.Now`) then every supplied `Option` in order; used internally by `Audit`, `Update`, `Status`, and `Dashboard`.

#### `ClusterProvisionerFactory`

??? abstract "`ClusterProvisionerFactory` — builds a provisioner per cluster"

    ```go
    type ClusterProvisionerFactory func(ctx context.Context, provider core.Provider, region string) (provisioner.ClusterProvisioner, error)
    ```

    Defined in [`fleet.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/fleet.go).

    - **Purpose**: builds the `ClusterProvisioner` for one cluster's provider and region. The CLI supplies one that constructs real cloud SDK clients (`internal/provisioner/{aws,gcp,azure}`); tests supply a fake — the same seam `internal/cli`'s `buildCloud` exists behind for `apply`.

#### `AuditResult`

??? abstract "`AuditResult` — one cluster's audit outcome"

    ```go
    type AuditResult struct {
        ClusterID core.ClusterID
        Findings  []Finding
        Err       error
    }
    ```

    Defined in [`fleet.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/fleet.go).

    - **Purpose**: either `Findings`, or `Err` set to an error that kept the audit from completing for that cluster. `Audit` returns one `AuditResult` per cluster in the registry listing so one cluster's failure (an unreachable cloud API, a repo that 404s) never aborts the whole run.

#### `Audit`

??? note "`Audit` — fans out `AuditOne` across all matching clusters"

    ```go
    func Audit(
        ctx context.Context, reg registry.Registry, filter registry.Filter,
        clusters ClusterProvisionerFactory, repoProv repo.Provisioner, concurrency int, opts ...Option,
    ) ([]AuditResult, error)
    ```

    Defined in [`fleet.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/fleet.go).

    - **Params**: `reg` — the Fleet Registry to list clusters from and record findings back to; `filter` — provider/phase filter applied to the registry listing; `clusters` — factory building a `ClusterProvisioner` per cluster's provider/region; `repoProv` — repository access for reading each cluster's `cluster.yaml`; `concurrency` — worker-pool bound, clamped to at least 1; `opts` — `Option`s (logger, clock).
    - **Returns**: one `AuditResult` per listed cluster (in registry-list order), or an error only if `reg.List` itself fails.
    - **Behavior**: lists clusters matching `filter`, then fans out `auditRecord` across a semaphore-bounded worker pool (`concurrency` in-flight at once), waiting for all to finish before returning. One cluster's failure never stops the others — each result is independent.
    - **`auditRecord`** (unexported): builds the cluster's provisioner, calls `AuditOne`, and — only when the audit itself succeeds — persists findings (even an empty list) to the registry via `reg.RecordFindings(ctx, rec.ClusterID, details, now())`. A `RecordFindings` failure is reported as that cluster's `AuditResult.Err`, since the audit ran but its result did not durably land anywhere fleet-wide tooling can read back.

## audit.go

#### `Finding`

??? abstract "`Finding` — one drift against `cluster.yaml`"

    ```go
    type Finding struct {
        ClusterID core.ClusterID
        Detail    string
    }
    ```

    Defined in [`audit.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/audit.go).

    - **Purpose**: one drift a cluster's live infra shows against its `cluster.yaml`. `AuditOne` produces zero or more of these; an empty slice means "clean," which `RecordFindings` distinguishes from "never audited" by writing an empty details list and a fresh `FindingsAt` timestamp.

#### `AuditOne`

??? note "`AuditOne` — diffs live state against desired `cluster.yaml`"

    ```go
    func AuditOne(
        ctx context.Context, cluster provisioner.ClusterProvisioner, repoProv repo.Provisioner, id core.ClusterID, provider core.Provider, region string,
    ) ([]Finding, error)
    ```

    Defined in [`audit.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/audit.go).

    - **Params**: `cluster` — provisioner for the cluster's live cloud state; `repoProv` — repository access for the desired `cluster.yaml`; `id`, `provider`, `region` — identify the cluster.
    - **Returns**: the drift `Finding`s (nil/empty if clean), or an error if `Describe` or reading/parsing `cluster.yaml` fails.
    - **Behavior**: calls `cluster.Describe` for live state. If `state.Status == provisioner.StatusAbsent`, returns a single finding: "registered in the Fleet Registry but does not exist in the cloud." Otherwise reads desired state via `desiredSpec` and compares:
        - `Access` mismatch produces one finding.
        - Each desired node pool missing from live state, or present but differing in `MinSize`/`MaxSize`/`DesiredSize`, produces one finding each.
    - **Invariant**: read-only throughout — never calls `Reconcile` or `Push`.

#### `desiredSpec`

??? note "`desiredSpec` (unexported, shared with `update.go`) — parses `cluster.yaml`"

    ```go
    func desiredSpec(ctx context.Context, repoProv repo.Provisioner, spec core.ClusterSpec) (core.ClusterSpec, error)
    ```

    Defined in [`audit.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/audit.go).

    - **Behavior**: clones the cluster's repository via `repoProv.Clone`, reads `repo.ClusterFile` (`cluster.yaml`) from the checkout, and YAML-unmarshals it into a `core.ClusterSpec`. Errors if the clone fails, the file is absent, or it fails to parse.
    - **Used by**: both `AuditOne` (to get the diff target) and `updateRecord` (to get the cluster's existing override patch before mutating it).

## status.go

#### `ClusterStatus`

??? abstract "`ClusterStatus` — one row in `fleet status`"

    ```go
    type ClusterStatus struct {
        ClusterID      core.ClusterID
        Provider       core.Provider
        Phase          core.Phase
        Stale          bool
        LastReportedAt time.Time

        FindingsCount int
        FindingsAt    time.Time
    }
    ```

    Defined in [`status.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/status.go).

    - **Purpose**: one cluster's row in a `fleet status` report. `FindingsCount` and `FindingsAt` reflect the cluster's most recent `fleet audit` run, read straight off the Fleet Registry record. `FindingsAt` zero means the cluster has never been audited — distinct from `FindingsCount == 0`, which means the last audit found no drift.

#### `Status`

??? note "`Status` — builds status rows from registry data alone"

    ```go
    func Status(
        ctx context.Context, reg registry.Registry, filter registry.Filter, staleOnly bool, threshold time.Duration, now time.Time,
        opts ...Option,
    ) ([]ClusterStatus, error)
    ```

    Defined in [`status.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/status.go).

    - **Params**: `reg`, `filter` — as above; `staleOnly` — when true, omits non-stale clusters from the result; `threshold` — how long since `LastReportedAt` before a `ready`-phase cluster is considered stale (see `DefaultStaleThreshold`); `now` — the reference time for staleness, injected for testability; `opts` — `Option`s.
    - **Returns**: one `ClusterStatus` per matching cluster (filtered by `staleOnly` if set), or an error if `reg.List` fails.
    - **Behavior**: lists clusters, computes `rec.Stale(now, threshold)` per record, and builds a `ClusterStatus` row from registry fields alone — `Phase`, `Provider`, `LastReportedAt`, and the most recent audit's `FindingsCount`/`FindingsAt`.
    - **Invariant**: never issues a call into a cluster or a cloud API; staleness is a pure function of registry data.

## update.go

#### `UpdateResult`

??? abstract "`UpdateResult` — one cluster's outcome from an update wave"

    ```go
    type UpdateResult struct {
        ClusterID core.ClusterID
        Committed bool
        Err       error

        Skipped bool
    }
    ```

    Defined in [`update.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/update.go).

    - **Purpose**: one cluster's outcome from an update wave. `Skipped` is true when the canary wave failed and this cluster's own update never ran because of it — the whole point of canarying is to find a bad version out before touching the rest of the fleet.

#### `UpdateOne`

??? note "`UpdateOne` — pins a version and commits for one cluster"

    ```go
    func UpdateOne(
        ctx context.Context, resolver catalog.Resolver, repoProv repo.Provisioner,
        spec core.ClusterSpec, component, version string,
    ) (bool, error)
    ```

    Defined in [`update.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/update.go).

    - **Params**: `resolver` — resolves `spec.Profile` to a `catalog.Profile`; `repoProv` — repository access for committing `addons.yaml`; `spec` — the cluster's spec, including its existing `Overrides`; `component`, `version` — the addon and version to pin.
    - **Returns**: `committed` — true if a commit was pushed; `false` means the cluster was already at that version; error on resolve/merge/commit failure.
    - **Behavior**: pins `component`'s version into `spec.Overrides` via `setComponentVersion`, then resolves the profile through `catalog.ResolveForCluster(ctx, resolver, spec)` — the same resolve → `ForProvider` → argocd-stand-in → `Merge` → ingress-template sequence `apply` uses — and commits via `repo.ReconcileAddons`. Reuses `catalog.ResolveForCluster` and `repo.ReconcileAddons` rather than duplicating their logic — an update wave is just many clusters each getting the same one-addon override applied, on top of whatever override patch they already carry, resolved identically to how `apply` would resolve it.

#### `setComponentVersion`

??? note "`setComponentVersion` (unexported) — pins a component's version"

    ```go
    func setComponentVersion(overrides []core.AddonOverride, component, version string) []core.AddonOverride
    ```

    Defined in [`update.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/update.go).

    - **Behavior**: returns a copy of `overrides` with `component`'s version pinned: updates the version in place if an override for `component` already exists, otherwise appends a new `core.AddonOverride{Name: component, Version: version}`. Leaves every other override untouched and never mutates the input slice.

#### `Update`

??? note "`Update` — rolls a version out fleet-wide, canary wave first"

    ```go
    func Update(
        ctx context.Context, reg registry.Registry, filter registry.Filter,
        resolver catalog.Resolver, repoProv repo.Provisioner,
        component, version string, concurrency, canaryCount int, opts ...Option,
    ) ([]UpdateResult, error)
    ```

    Defined in [`update.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/update.go).

    - **Params**: `reg`, `filter`, `resolver`, `repoProv` — as above; `component`, `version` — the update to roll out; `concurrency` — worker-pool bound per wave, clamped to at least 1; `canaryCount` — size of the canary wave, clamped to `[0, len(records)]`; `opts` — `Option`s.
    - **Returns**: one `UpdateResult` per cluster (canary wave first, then either the rest wave or `Skipped: true` entries), or an error only if `reg.List` fails.
    - **Behavior**: lists clusters and sorts them deterministically by `ClusterID` so a canary wave is reproducible run to run. Splits into `canary` (first `canaryCount`) and `rest`. Runs `updateWave` on the canary first, if non-empty.
    - **Invariant**: if any canary result has `Err != nil`, every remaining cluster is reported `Skipped: true` and `Update` returns immediately — canarying exists specifically to catch a bad version before it reaches the whole fleet, so continuing past a canary failure would defeat the point. Otherwise runs `updateWave` on `rest` and appends those results. Like `Audit`, one cluster's failure within a wave (a rate-limited GitHub API, a malformed override) does not abort the rest of that wave.

#### `countFailedUpdates`

??? note "`countFailedUpdates` (unexported) — counts errored results"

    ```go
    func countFailedUpdates(results []UpdateResult) int
    ```

    Defined in [`update.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/update.go).

    - **Behavior**: counts results with `Err != nil`. Used by `Update` to decide whether the canary wave failed.

#### `updateWave`

??? note "`updateWave` (unexported) — runs `updateRecord` across a wave"

    ```go
    func updateWave(
        ctx context.Context, records []registry.Record, resolver catalog.Resolver, repoProv repo.Provisioner,
        component, version string, concurrency int, logger *slog.Logger,
    ) []UpdateResult
    ```

    Defined in [`update.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/update.go).

    - **Behavior**: runs `updateRecord` across `records` on a semaphore-bounded worker pool, same pattern as `Audit`'s fan-out. `Update` stages this twice — once for the canary wave, once for the rest of the fleet — so both waves get identical worker-pool and per-cluster-failure behavior.

#### `updateRecord`

??? note "`updateRecord` (unexported) — updates one cluster's addon override"

    ```go
    func updateRecord(
        ctx context.Context, rec registry.Record, resolver catalog.Resolver, repoProv repo.Provisioner, component, version string,
        logger *slog.Logger,
    ) UpdateResult
    ```

    Defined in [`update.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/update.go).

    - **Behavior**: reads the cluster's existing `cluster.yaml` via `desiredSpec` before updating it — the Fleet Registry does not carry a cluster's override patch, only its repository does, so skipping this read would replace the existing overrides with one containing only this update's override. Then calls `UpdateOne` and logs the outcome (`Info` on an actual commit, since a push to a cluster's repository is a fleet-wide mutation worth seeing without `--log-level debug`; `Debug` otherwise).

## dashboard.go

#### `DashboardRow`

??? abstract "`DashboardRow` — one row in `fleet dashboard`"

    ```go
    type DashboardRow struct {
        ClusterID      core.ClusterID
        Provider       core.Provider
        Phase          core.Phase
        Stale          bool
        LastReportedAt time.Time
        Findings       []string
        FindingsAt     time.Time
    }
    ```

    Defined in [`dashboard.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/dashboard.go).

    - **Purpose**: one cluster's row in `fleet dashboard`: sync/phase status, staleness, and the full drift findings from its most recent `fleet audit` run, all read straight off its Fleet Registry record and correlated by `ClusterID`.
    - **Note**: a commit SHA is deliberately not part of this: the Fleet Registry does not track one (a cluster's resolved addons and `.state.yaml` live in its own repository, not centrally), so correlating by SHA would mean a repository read per cluster that nothing else this command does requires. `ClusterID` is the correlation key the registry and every cluster's own repository already share.

#### `Dashboard`

??? note "`Dashboard` — builds dashboard rows from registry data alone"

    ```go
    func Dashboard(
        ctx context.Context, reg registry.Registry, filter registry.Filter, threshold time.Duration, now time.Time,
        opts ...Option,
    ) ([]DashboardRow, error)
    ```

    Defined in [`dashboard.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/dashboard.go).

    - **Params**: same shape as `Status`, minus `staleOnly`.
    - **Returns**: one `DashboardRow` per matching cluster, sorted by `ClusterID` so a rendered report is stable run to run; error only if `reg.List` fails.
    - **Behavior**: like `Status`, reads only the registry — never connects to a cluster. Each row carries the full `Findings []string` from the cluster's last `fleet audit`, not just a count, which is what distinguishes it from `Status`.

## Invariants

- **All reads and writes go through `internal/registry`.** No function in this package makes a raw SQL (or other SDK) call directly — every fleet-wide view starts with `reg.List(ctx, filter)`.
- **`Status` and `Dashboard` never reach into a cluster.** They compute everything — phase, staleness, findings — from the registry record. This is what keeps a `fleet status` run from hanging on one unreachable cluster: a gap in `LastReportedAt` is a real signal (*stale*), not a timeout.
- **`Audit` is read-only against cloud infra**: it calls `Describe`, never `Reconcile`, and calls `Clone`, never `Push`.
- **`Update` is the only writer**, and only to each cluster's repository (`addons.yaml` via `repo.ReconcileAddons`) — never to cloud infra directly.
- **Per-cluster failure isolation.** `Audit` and each wave of `Update` fan out over a bounded worker pool and record one outcome per cluster; a single cluster's error never aborts the batch (the sole exception is a failed canary wave in `Update`, which is a deliberate design choice, not an incidental failure propagation).
