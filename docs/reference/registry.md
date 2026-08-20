# internal/registry

`internal/registry` is the client for the Fleet Registry — the single source of durable fleet state, keyed by `ClusterID`. Every other component (`internal/orchestrator`, `internal/fleet`, `internal/cli`) reads and writes cluster state exclusively through this package's `Registry` interface rather than issuing raw SQL directly, so the invariants that keep the fleet consistent (illegal phase transitions rejected, stale-version writes rejected, leases exclusive) live in one place. The production implementation is `Postgres` (`postgres.go`), backed by a single `fleet_registry` table that it creates and migrates idempotently on connect — there is no separate migration step or provisioning command for it. The lease itself is a time-bounded claim (`Lease{Holder, ExpiresAt}`) stored on the same row: `AcquireLease` performs a conditional `UPDATE` that only succeeds when the lease is unset, already owned by the caller, or expired — this serializes concurrent `apply` runs against the same cluster while letting a crashed run self-heal once its TTL passes rather than wedging the cluster forever.

## Quick reference

| Name | Kind | File | Summary |
|---|---|---|---|
| [`Lease`](#lease) | struct | registry.go | Time-bounded claim on a cluster; prevents two concurrent `apply` runs on the same cluster. |
| [`Record`](#record) | struct | registry.go | One cluster's row in the registry — the durable half of a cluster's state. |
| [`Filter`](#filter) | struct | registry.go | Narrows a `List` call; a zero `Filter` matches every cluster. |
| [`Registry`](#registry-interface) | interface | registry.go | The durable store of fleet state — the contract both `Postgres` and `Memory` implement identically. |
| [`ArgoCDAccess`](#argocdaccess) | struct | registry.go | A cluster's Argo CD connection details — endpoint, admin username/password, kube context. |
| [`NewRecord`](#record) | func | registry.go | Builds a `PhasePending` record from a validated `ClusterSpec`, `Version` seeded at 1. |
| [Sentinel errors](#sentinel-errors) | vars | registry.go | `ErrNotFound`, `ErrAlreadyExists`, `ErrVersionConflict`, `ErrLeaseHeld`, `ErrLeaseLost`. |
| [`Postgres`](#postgres) | struct | postgres.go | Production `Registry`, backed by a Postgres `fleet_registry` table it creates and migrates itself. |
| [`Option`](#option-and-withlogger) | type (func) | postgres.go | Functional option for `NewPostgres`. |
| [`WithLogger`](#option-and-withlogger) | func | postgres.go | `Option` that overrides the logger used by `Postgres`. |
| [`NewPostgres`](#newpostgres) | func | postgres.go | Opens a connection pool against a DSN, pings it, and idempotently ensures the schema exists. |
| [`Memory`](#memory) | struct | memory.go | In-memory `Registry`, so components built on the registry are testable without credentials or a container. |
| [`MemoryOption`](#memoryoption-and-withclock) | type (func) | memory.go | Functional option for `NewMemory`. |
| [`WithClock`](#memoryoption-and-withclock) | func | memory.go | `MemoryOption` that replaces the time source, so lease expiry is testable without sleeping. |
| [`NewMemory`](#newmemory) | func | memory.go | Constructs an empty `Memory` registry. |

## registry.go

### `Lease`

??? abstract "Signature"

    ```go
    type Lease struct {
        Holder    string
        ExpiresAt time.Time
    }

    func (l Lease) Expired(now time.Time) bool
    ```

- **Purpose:** a time-bounded claim on a cluster, preventing two concurrent `apply` runs from provisioning the same cluster at once.
- **Invariants:** expires rather than being held indefinitely — `Expired` is the sole check used both client-side (`Record.Held`) and to distinguish a stale lease from a live conflict during acquisition.

### `Record`

??? abstract "Signature"

    ```go
    type Record struct {
        ClusterID core.ClusterID
        Phase     core.Phase

        Provider core.Provider
        Region   string
        Access   core.Access
        Profile  core.ProfileRef

        OIDCIssuer string

        Version int64

        LastReportedAt time.Time

        Findings   []string
        FindingsAt time.Time

        CreatedAt time.Time
        UpdatedAt time.Time

        Lease *Lease
    }

    func (r Record) Stale(now time.Time, threshold time.Duration) bool
    func (r Record) Held(now time.Time) bool
    func (r Record) Validate() error
    func NewRecord(spec core.ClusterSpec, now time.Time) Record
    ```

- **Purpose:** one cluster's row in the registry — the durable half of a cluster's state. The other half (resolved addons, node pool detail) lives in the cluster's own repository; `Record` holds only what the fleet needs to reason about centrally.
- **Fields:**
    - `OIDCIssuer` — recorded once identity binding (M2) succeeds; the Central Ingestion API verifies `fleet-status-reporter`'s signature against exactly this issuer, which is what makes a signature from one cluster unusable to spoof another.
    - `Version` — bumped on every data write and asserted as a condition, so a racing read-modify-write fails instead of overwriting.
    - `Findings` / `FindingsAt` — the drift `fleet audit` last found. An empty, non-nil slice with a non-zero `FindingsAt` means the cluster was audited and found clean; a zero `FindingsAt` means never audited. Audit is the only writer — `apply`/`delete` never touch this field.
    - `Lease` — nil when unheld; an expired lease may still be present until the next acquisition overwrites it.
- **Behavior (methods):**
    - `Stale(now, threshold)` — true only for a `PhaseReady` cluster that has missed its reporting window, judged from `LastReportedAt` (or `CreatedAt` if it has never reported). A statement about missing reports, never about reachability — nothing in this package connects to a cluster.
    - `Held(now)` — true when `Lease` is non-nil and unexpired at `now`.
    - `Validate()` — checks `ClusterID`, `Phase.Valid()`, `Provider.Valid()`, non-empty `Region`, and `Access.Valid()`, joining all failures with `errors.Join` and wrapping each in `core.ErrInvalidSpec`.
- **Functions:**
    - `NewRecord(spec core.ClusterSpec, now time.Time) Record` — builds a `PhasePending` record from a validated `ClusterSpec`, `Version` seeded at 1.

### `ArgoCDAccess`

??? abstract "Signature"

    ```go
    type ArgoCDAccess struct {
        Provider    core.Provider
        Region      string
        KubeContext string
        Endpoint    string // argocd-server LB external IP or hostname, no scheme
        Username    string
        Password    string // plaintext
    }
    ```

- **Purpose:** a cluster's Argo CD connection details, captured by `kubespin apply` (`internal/cli/apply.go`'s `captureAndRecordArgoCDAccess`) once the cluster reaches `PhaseReady` and the `argocd-server` LoadBalancer Service has an assigned endpoint. It is observational metadata, like `Record.OIDCIssuer`, not part of the phase state machine — capture runs on every apply that reaches ready, including a no-op reconcile against an already-ready cluster, so a failed capture simply gets another chance on the next run.
- **Invariant:** `Password` is stored in **plaintext**, matching the trust model already extended to `KUBESPIN_REGISTRY_DSN` (an operator-supplied, non-flag secret). There is no separate secrets-manager integration for it.
- **Storage:** persisted in a Postgres child table, `cluster_argocd_details`, one row per cluster (upserted, not appended), foreign-keyed to `fleet_registry(cluster_id)` with `ON DELETE CASCADE` so a decommissioned cluster's row disappears with it. `provider`/`region` are denormalized from `fleet_registry` so ad hoc `psql` queries don't need a join.

### `Filter`

??? abstract "Signature"

    ```go
    type Filter struct {
        Provider core.Provider
        Phase    core.Phase
    }
    ```

- **Purpose:** narrows a `List` call. A zero `Filter` matches every cluster.
- **Invariant:** `Postgres.List` is always one query, `WHERE ($1 = '' OR provider = $1) AND ($2 = '' OR phase = $2)`, served by the `fleet_registry_provider_phase_idx` index (on `(provider, phase)`, created with the table) whenever `Provider` is set — unlike the eventually-consistent scan-vs-GSI-query choice a DynamoDB-backed registry would face, Postgres reads are always consistent, so there is no separate index-or-scan code path to choose between.

### `Registry` (interface)

??? abstract "Signature"

    ```go
    type Registry interface {
        Get(ctx context.Context, id core.ClusterID) (Record, error)
        Create(ctx context.Context, rec Record) (Record, error)
        UpdatePhase(ctx context.Context, rec Record, to core.Phase) (Record, error)
        Touch(ctx context.Context, id core.ClusterID, at time.Time) error
        RecordOIDCIssuer(ctx context.Context, id core.ClusterID, issuer string) error
        RecordFindings(ctx context.Context, id core.ClusterID, findings []string, at time.Time) error
        List(ctx context.Context, filter Filter) ([]Record, error)
        AcquireLease(ctx context.Context, id core.ClusterID, holder string, ttl time.Duration) (Lease, error)
        RenewLease(ctx context.Context, id core.ClusterID, holder string, ttl time.Duration) (Lease, error)
        ReleaseLease(ctx context.Context, id core.ClusterID, holder string) error
        RecordArgoCDAccess(ctx context.Context, id core.ClusterID, access ArgoCDAccess) error
        GetArgoCDAccess(ctx context.Context, id core.ClusterID) (ArgoCDAccess, error)
    }
    ```

- **Purpose:** the durable store of fleet state — the contract both `Postgres` and `Memory` implement identically.
- **Invariants implementations must enforce** (callers rely on these rather than re-checking):
    - `UpdatePhase` rejects an illegal transition with `ErrInvalidTransition` (checked against `core.ValidateTransition`), and rejects a write against a stale `Phase`/`Version` pair with `ErrVersionConflict`.
    - `Touch`, `RecordOIDCIssuer`, `RecordFindings`, and `RecordArgoCDAccess` carry **no** version check — heartbeats, identity-issuer recording, audit findings, and Argo CD access details are metadata writes that must not contend with an in-flight phase transition.
    - `RecordArgoCDAccess` upserts (repeat calls replace the previous values); `GetArgoCDAccess` returns `ErrNotFound` if nothing has been captured yet for the cluster.
    - `AcquireLease` fails with `ErrLeaseHeld` if another holder's lease is still valid; an expired lease is taken over without ceremony.
    - `RenewLease` fails with `ErrLeaseLost` if the caller's lease already expired — silently re-acquiring here would defeat the lock, since another holder may already own it.
    - `ReleaseLease` fails with `ErrLeaseLost` if the lease is held by someone else.

### Sentinel errors

??? note "Signature"

    ```go
    var (
        ErrNotFound        = errors.New("cluster not found in registry")
        ErrAlreadyExists    = errors.New("cluster already exists in registry")
        ErrVersionConflict  = errors.New("registry record was modified concurrently")
        ErrLeaseHeld        = errors.New("cluster lease is held by another holder")
        ErrLeaseLost        = errors.New("cluster lease is no longer held by this holder")
    )
    ```

- **Behavior:** callers branch with `errors.Is` rather than matching messages.

## postgres.go

### Schema

??? note "`schemaDDL` / `selectColumns`"

    ```go
    const schemaDDL = `
    CREATE TABLE IF NOT EXISTS fleet_registry (
        cluster_id        TEXT PRIMARY KEY,
        phase             TEXT NOT NULL,
        provider          TEXT NOT NULL,
        region            TEXT NOT NULL,
        access            TEXT NOT NULL,
        profile_name      TEXT NOT NULL,
        profile_version   TEXT NOT NULL,
        oidc_issuer       TEXT NOT NULL DEFAULT '',
        version           BIGINT NOT NULL,
        last_reported_at  TIMESTAMPTZ,
        findings          JSONB,
        findings_at       TIMESTAMPTZ,
        created_at        TIMESTAMPTZ NOT NULL,
        updated_at        TIMESTAMPTZ NOT NULL,
        lease_holder      TEXT,
        lease_expires_at  TIMESTAMPTZ
    );
    CREATE INDEX IF NOT EXISTS fleet_registry_provider_phase_idx ON fleet_registry (provider, phase);
    `
    ```

    - **Behavior:** run by `NewPostgres` on every connect, via `CREATE TABLE IF NOT EXISTS`/`CREATE INDEX IF NOT EXISTS`, so a fresh database is ready without a separate migration step and a run against an already-provisioned one is a no-op. It only ever adds — there is no `DROP`/`ALTER` anywhere in this package.
    - **Invariant:** `cluster_id` alone is the primary key, deliberately — this is what makes `AcquireLease` (a conditional `UPDATE` on that same row) actually serialize a status report against a concurrent phase transition; a composite key would let them proceed independently and the lock would protect nothing.
    - `selectColumns` is a single shared column list used by every read (`Get`, `List`, and `UpdatePhase`'s `RETURNING`), so a column can't drift between them.

??? note "`argoCDDetailsDDL`"

    ```go
    const argoCDDetailsDDL = `
    CREATE TABLE IF NOT EXISTS cluster_argocd_details (
        cluster_id       TEXT PRIMARY KEY REFERENCES fleet_registry(cluster_id) ON DELETE CASCADE,
        provider         TEXT NOT NULL,
        region           TEXT NOT NULL,
        kube_context     TEXT NOT NULL,
        argocd_endpoint  TEXT NOT NULL,
        argocd_username  TEXT NOT NULL,
        argocd_password  TEXT NOT NULL,
        captured_at      TIMESTAMPTZ NOT NULL,
        updated_at       TIMESTAMPTZ NOT NULL
    );
    `
    ```

    - **Behavior:** a second migration, run by `NewPostgres` right after `schemaDDL`, following the same self-migration-on-connect convention. One row per cluster (upsert, not append). `captured_at` is set only by the `INSERT` branch of `RecordArgoCDAccess`'s `ON CONFLICT ... DO UPDATE` and never moves on a later upsert; `updated_at` bumps on every call.
    - **Invariant:** every column is `NOT NULL` — a capture attempt that cannot obtain all six fields (endpoint, username, password, kube context, provider, region) writes no row at all, so there is no partial/incomplete state to reason about.

### `Postgres`

??? abstract "Signature"

    ```go
    type Postgres struct {
        db     *sql.DB
        now    func() time.Time
        logger *slog.Logger
    }
    ```

- **Purpose:** the production `Registry`, backed by the `fleet_registry` table it creates and migrates itself — there is no separate provisioning step for it (unlike the ingestion Lambda/API Gateway, which `fleet bootstrap` does provision).
- **Behavior:**
    - Uses `database/sql` over the `pgx` driver (`github.com/jackc/pgx/v5/stdlib`, imported for its side-effecting driver registration), not a Postgres-specific client library — so the registry is testable against any `database/sql`-compatible backend.
    - Registry logging is Debug-level diagnostic detail except a lease conflict, logged at Warn — that is the exact race the lease exists to catch.

### `Option` and `WithLogger`

??? note "Signature"

    ```go
    type Option func(*Postgres)

    func WithLogger(logger *slog.Logger) Option
    ```

- **Params:** `logger *slog.Logger` — the logger `Postgres` should use.
- **Behavior:** `Option` is a functional option for `NewPostgres`; `WithLogger` overrides the default logger (ignoring a nil logger, so passing one is optional).

### `NewPostgres`

??? note "Signature"

    ```go
    func NewPostgres(ctx context.Context, dsn string, opts ...Option) (*Postgres, error)
    ```

- **Params:** `ctx context.Context`; `dsn string` — a `postgres://` connection string, read by callers only from `KUBESPIN_REGISTRY_DSN` (there is deliberately no flag for it, so a connection string carrying a password never lands in shell history); `opts ...Option`.
- **Behavior:** `sql.Open("pgx", dsn)`, then `PingContext` to verify the connection actually works (rather than deferring the first error to whatever query happens to run first), then executes `schemaDDL`. `now` defaults to `time.Now` and `logger` to `slog.Default()`, both overridable via `Option`.

### Method behavior (Postgres)

??? note "`Get`"

    - `SELECT` by `cluster_id`, scanned via the shared `scanRecord` helper.
    - Returns `ErrNotFound` when `sql.ErrNoRows`.

??? note "`Create`"

    - Validates the record, defaults `Version` to 1, encodes `Findings` (`findingsJSON` — `nil`/SQL `NULL` when `FindingsAt` is zero, so "never audited" stays distinct from an empty, encoded `[]`), then `INSERT ... ON CONFLICT (cluster_id) DO NOTHING`.
    - `RowsAffected() == 0` (the row already existed, so the `ON CONFLICT` clause suppressed the insert) maps to `ErrAlreadyExists`.

??? note "`UpdatePhase`"

    - Rejects the transition client-side via `core.ValidateTransition` before any query.
    - Then `UPDATE fleet_registry SET phase = $1, version = version + 1, updated_at = $2 WHERE cluster_id = $3 AND phase = $4 AND version = $5 RETURNING <selectColumns>` — asserting **both** phase and version, so a racing writer that already advanced the record loses this write instead of silently overwriting it.
    - On `sql.ErrNoRows` (the `WHERE` matched nothing), a follow-up `Get` distinguishes not-found from a genuine version conflict — the same two-case split DynamoDB's `ReturnValuesOnConditionCheckFailure` gave for free on a condition failure; here it costs a second read instead.

??? note "`Touch`"

    - `UPDATE fleet_registry SET last_reported_at = $1 WHERE cluster_id = $2`.
    - **Invariant:** deliberately no version check, so frequent heartbeats never contend with a phase transition in progress.
    - **Implementation:** delegates to the shared `execNoVersionCheck` helper (`postgres.go`), which runs the `UPDATE` and maps `RowsAffected() == 0` to `ErrNotFound` once for all three no-version-check writes below.

??? note "`RecordOIDCIssuer`"

    - Same no-version-check pattern as `Touch` (via `execNoVersionCheck`), sets `oidc_issuer` once.

??? note "`RecordFindings`"

    - Same no-version-check pattern (via `execNoVersionCheck`), sets `findings` and `findings_at` together, replacing whatever was recorded before.

??? note "`RecordArgoCDAccess`"

    - `INSERT INTO cluster_argocd_details (...) VALUES (...) ON CONFLICT (cluster_id) DO UPDATE SET ...` — every column except `captured_at` is refreshed on conflict; `captured_at` is only ever set by the `INSERT` branch, so it stays fixed across repeat calls while `updated_at` advances.
    - A foreign-key violation (the referenced `fleet_registry` row doesn't exist — checked via `errors.As` against `*pgconn.PgError` with SQLSTATE `23503`) maps to `ErrNotFound`, the same as every other write against a nonexistent cluster.

??? note "`GetArgoCDAccess`"

    - `SELECT provider, region, kube_context, argocd_endpoint, argocd_username, argocd_password FROM cluster_argocd_details WHERE cluster_id = $1`.
    - Returns `ErrNotFound` on `sql.ErrNoRows` — covers both "no such cluster" and "cluster exists but nothing has been captured yet", since the table only ever has entries once `RecordArgoCDAccess` has succeeded at least once.

??? note "`List`"

    - One query, always: `SELECT <selectColumns> FROM fleet_registry WHERE ($1 = '' OR provider = $1) AND ($2 = '' OR phase = $2) ORDER BY cluster_id`, served by the `(provider, phase)` index whenever `Provider` is set.
    - Unlike a DynamoDB-backed registry's eventually-consistent scan-vs-GSI-query split, there is exactly one code path here — Postgres reads are always consistent, so there is nothing to choose between or paginate through beyond what the driver already does.

??? note "`AcquireLease`"

    - `UPDATE fleet_registry SET lease_holder = $1, lease_expires_at = $2 WHERE cluster_id = $3 AND (lease_holder IS NULL OR lease_expires_at <= $4 OR lease_holder = $1)` — succeeds when the lease is free, expired, or already owned by `holder`. The `<=` matches `Lease.Expired()`'s `!now.Before(expiresAt)` exactly, so "expired" means the same instant here as everywhere else that reasons about a lease.
    - `RowsAffected() == 0` calls the shared `leaseConflict` helper, which does a follow-up `Get` to distinguish `ErrNotFound` (no such cluster) from `ErrLeaseHeld` (a real conflict, logged at Warn — the exact race the lease exists to catch).

??? note "`RenewLease`"

    - `UPDATE ... SET lease_expires_at = $1 WHERE cluster_id = $2 AND lease_holder = $3 AND lease_expires_at > $4` — strictly greater than now, so an already-expired lease cannot be renewed (another holder may already own it).
    - `RowsAffected() == 0` maps through `leaseConflict` to `ErrLeaseLost` (or `ErrNotFound` if the cluster itself is gone).

??? note "`ReleaseLease`"

    - `UPDATE ... SET lease_holder = NULL, lease_expires_at = NULL WHERE cluster_id = $1 AND lease_holder = $2`.
    - `RowsAffected() == 0` maps through `leaseConflict` to `ErrLeaseLost`/`ErrNotFound` the same way.

### Helper functions

??? note "`leaseConflict`"

    ```go
    func (p *Postgres) leaseConflict(ctx context.Context, id core.ClusterID, sentinel error) error
    ```

    - **Behavior:** a follow-up `Get` that distinguishes "no such cluster" (`ErrNotFound`) from a genuine lease conflict (`sentinel`, wrapped with the current holder if present) — mirroring the item DynamoDB would have returned alongside a failed condition for free; here it costs the extra read.

??? note "`scanRecord`"

    ```go
    type rowScanner interface {
        Scan(dest ...any) error
    }

    func scanRecord(s rowScanner) (Record, error)
    ```

    - **Behavior:** `rowScanner` is satisfied by both `*sql.Row` and `*sql.Rows`, so this one function serves `Get`/`UpdatePhase` (one row) and `List` (many) alike. Builds a `Record` from the scanned columns, using `sql.NullTime`/`sql.NullString` for the optional ones (`LastReportedAt`, `FindingsAt`, lease fields) so "never reported"/"never audited" stays distinguishable from the zero time rather than colliding with it. `Findings` is only unmarshalled when `findings_at` is valid — an absent `FindingsAt` (never audited) must stay distinguishable from an empty `Findings` list (audited and clean). Returns an error if a lease holder is set but its expiry is `NULL` — a state the schema allows but the application logic must never produce.

??? note "`findingsJSON` / `nullTime` / `leaseHolder` / `leaseExpiry`"

    ```go
    func findingsJSON(rec Record) (any, error)
    func nullTime(t time.Time) any
    func leaseHolder(l *Lease) any
    func leaseExpiry(l *Lease) any
    ```

    - **Behavior:** small helpers shared by `Create`, converting Go zero values into SQL `NULL` (rather than an empty/zero value that would collapse "never reported"/"never audited"/"unheld" into a real value) and a `*Lease` into its two column values.

## memory.go

### `Memory`

??? abstract "Signature"

    ```go
    type Memory struct {
        mu           sync.Mutex
        records      map[core.ClusterID]Record
        argocdAccess map[core.ClusterID]ArgoCDAccess
        now          func() time.Time
    }
    ```

- **Purpose:** an in-memory `Registry`, so every component built on the registry — the orchestrator above all — is testable without credentials or a container.
- **Invariant:** explicitly documented as not a simplified stand-in — it enforces exactly the same conditions as `Postgres`, and both implementations are exercised by the same contract test suite (`contract_test.go`, run against `Memory` unconditionally and against `Postgres` under the `integration` build tag — see [Development: the registry contract](../development.md#the-registry-contract)), so a fake with weaker semantics can't let real bugs pass.
- **Behavior:** `NewMemory` returns an empty registry guarded by a single `sync.Mutex`; every method takes the lock for its full body.

### `MemoryOption` and `WithClock`

??? note "Signature"

    ```go
    type MemoryOption func(*Memory)

    func WithClock(now func() time.Time) MemoryOption
    ```

- **Params:** `now func() time.Time` — replacement time source.
- **Behavior:** `WithClock` replaces the time source so lease expiry is testable without sleeping.

### `NewMemory`

??? note "Signature"

    ```go
    func NewMemory(opts ...MemoryOption) *Memory
    ```

- **Params:** `opts ...MemoryOption`.
- **Behavior:** returns an empty registry.

### `clone`

??? note "Signature"

    ```go
    func clone(rec Record) Record
    ```

- **Behavior:** deep-copies a record (specifically the `Lease` pointer) before returning it, so callers cannot mutate stored state through a pointer they were handed.

### Method behavior (Memory)

Mirrors `Postgres` exactly, implemented against the map instead of conditional `UPDATE` statements:

??? note "`Create`"

    - `ErrAlreadyExists` if the key exists; defaults `Version` to 1.

??? note "`UpdatePhase`"

    - Validates the transition via `core.ValidateTransition` first (failing before touching stored state, not persisting and cleaning up after).
    - Then compares both `stored.Version != rec.Version` and `stored.Phase != rec.Phase` before mutating — `ErrVersionConflict` otherwise.
    - On success bumps `Version`, sets `Phase`, and stamps `UpdatedAt` from the injected clock.

??? note "`Touch` / `RecordOIDCIssuer` / `RecordFindings` / `RecordArgoCDAccess` / `GetArgoCDAccess`"

    - The four writes mutate directly with no version bump, matching the "not a phase transition" reasoning in `postgres.go`. `RecordArgoCDAccess` requires the cluster to exist in `records` first (`ErrNotFound` otherwise, mirroring `Postgres`'s foreign-key check) and stores into the separate `argocdAccess` map, upserting by key. `GetArgoCDAccess` returns `ErrNotFound` if that map has no entry for the cluster.

??? note "`List`"

    - Filters in-memory by `Provider`/`Phase`, then sorts by `ClusterID` for deterministic output — `Postgres.List` gets the same ordering for free from its `ORDER BY cluster_id`, but the map here has none on its own, so it is added explicitly to match.

??? note "`AcquireLease`"

    - `ErrLeaseHeld` if `stored.Lease` is non-nil, unexpired, and held by a different holder; otherwise overwrites `stored.Lease`.

??? note "`RenewLease`"

    - `ErrLeaseLost` if there is no lease, it's held by someone else, or it has already expired.

??? note "`ReleaseLease`"

    - `ErrLeaseLost` if there is no lease or it's held by someone else; otherwise clears `stored.Lease`.
