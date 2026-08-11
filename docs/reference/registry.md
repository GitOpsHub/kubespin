# internal/registry

`internal/registry` is the client for the Fleet Registry — the single source of durable fleet state, keyed by `ClusterID`. Every other component (`internal/orchestrator`, `internal/fleet`, `internal/cli`) reads and writes cluster state exclusively through this package's `Registry` interface rather than issuing raw DynamoDB SDK calls, so the invariants that keep the fleet consistent (illegal phase transitions rejected, stale-version writes rejected, leases exclusive) live in one place. The lease itself is a time-bounded claim (`Lease{Holder, ExpiresAt}`) stored on the same record: `AcquireLease` performs a DynamoDB conditional `UpdateItem` that only succeeds when the lease is unset, already owned by the caller, or expired — this serializes concurrent `apply` runs against the same cluster while letting a crashed run self-heal once its TTL passes rather than wedging the cluster forever.

## Quick reference

| Name | Kind | File | Summary |
|---|---|---|---|
| [`Lease`](#lease) | struct | registry.go | Time-bounded claim on a cluster; prevents two concurrent `apply` runs on the same cluster. |
| [`Record`](#record) | struct | registry.go | One cluster's row in the registry — the durable half of a cluster's state. |
| [`Filter`](#filter) | struct | registry.go | Narrows a `List` call; a zero `Filter` matches every cluster. |
| [`Registry`](#registry-interface) | interface | registry.go | The durable store of fleet state — the contract both `DynamoDB` and `Memory` implement identically. |
| [`NewRecord`](#record) | func | registry.go | Builds a `PhasePending` record from a validated `ClusterSpec`, `Version` seeded at 1. |
| [Sentinel errors](#sentinel-errors) | vars | registry.go | `ErrNotFound`, `ErrAlreadyExists`, `ErrVersionConflict`, `ErrLeaseHeld`, `ErrLeaseLost`. |
| [`DynamoDB`](#dynamodb) | struct | dynamo.go | Production `Registry`, backed by the Fleet Registry table created by `fleet bootstrap`. |
| [`Option`](#option-and-withlogger) | type (func) | dynamo.go | Functional option for `NewDynamoDB`. |
| [`WithLogger`](#option-and-withlogger) | func | dynamo.go | `Option` that overrides the logger used by `DynamoDB`. |
| [`NewDynamoDB`](#newdynamodb) | func | dynamo.go | Constructs a `DynamoDB` registry client for a region/table. |
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

### `Filter`

??? abstract "Signature"

    ```go
    type Filter struct {
        Provider core.Provider
        Phase    core.Phase
    }
    ```

- **Purpose:** narrows a `List` call. A zero `Filter` matches every cluster.
- **Invariant:** setting `Provider` routes `DynamoDB.List` through the `ProviderPhaseIndex` GSI instead of a table scan — the index exists from the day the table does specifically so fleet-wide operations can filter by provider cheaply.

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
    }
    ```

- **Purpose:** the durable store of fleet state — the contract both `DynamoDB` and `Memory` implement identically.
- **Invariants implementations must enforce** (callers rely on these rather than re-checking):
    - `UpdatePhase` rejects an illegal transition with `ErrInvalidTransition` (checked against `core.ValidateTransition`), and rejects a write against a stale `Phase`/`Version` pair with `ErrVersionConflict`.
    - `Touch`, `RecordOIDCIssuer`, and `RecordFindings` carry **no** version check — heartbeats, identity-issuer recording, and audit findings are metadata writes that must not contend with an in-flight phase transition.
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

## item.go

Not exported, but load-bearing for the wire format.

??? note "Attribute name constants"

    - `attrClusterID`, `attrPhase`, `attrLeaseHolder`, `attrLeaseExpiresAt`, etc.
    - **Invariant:** only `ClusterID`, `Provider`, and `Phase` are part of the table's key schema or the `ProviderPhaseIndex` GSI; the rest are schemaless.

??? note "`epochMillis` / `fromEpochMillis`"

    ```go
    func epochMillis(t time.Time) string
    func fromEpochMillis(s string) (time.Time, error)
    ```

    - **Behavior:** lease expiry is stored as epoch milliseconds rather than an RFC3339 string.
    - **Invariant:** this is specifically so the `AcquireLease` condition can compare it with `<` numerically; RFC3339 strings are variable-width and would not order correctly under a string comparison.

??? note "`marshalRecord` / `unmarshalRecord`"

    ```go
    func marshalRecord(rec Record) map[string]types.AttributeValue
    func unmarshalRecord(item map[string]types.AttributeValue) (Record, error)
    ```

    - **Behavior:** convert between `Record` and the DynamoDB item shape.
    - **Invariant:** optional fields (`LastReportedAt`, `OIDCIssuer`, `Lease`, `Findings`/`FindingsAt`) are written **absent**, not as empty/zero values, when unset — e.g. "never reported" and "reported at the zero time" must not collapse into the same item, and an absent `FindingsAt` (never audited) must stay distinguishable from an empty `Findings` list (audited and clean).

??? note "`stringAttr` / `numberAttr`"

    - **Behavior:** typed attribute readers used throughout `unmarshalRecord`.

??? note "`key`"

    ```go
    func key(id core.ClusterID) map[string]types.AttributeValue
    ```

    - **Behavior:** builds the primary-key item for `GetItem`/`UpdateItem` calls.

## dynamo.go

### `DynamoDB`

??? abstract "Signature"

    ```go
    type DynamoDB struct {
        client dynamoAPI
        table  string
        now    func() time.Time
        logger *slog.Logger
    }
    ```

- **Purpose:** the production `Registry`, backed by the Fleet Registry table created by `fleet bootstrap`.
- **Behavior:**
    - `dynamoAPI` is a narrow interface (`GetItem`, `PutItem`, `UpdateItem`, `Query`, `Scan`) declared in this package, not the full DynamoDB client, so the registry is testable without credentials and its permission surface is explicit.
    - Registry logging is Debug-level diagnostic detail except a lease conflict, logged at Warn — that is the exact race the lease exists to catch.

### `Option` and `WithLogger`

??? note "Signature"

    ```go
    type Option func(*DynamoDB)

    func WithLogger(logger *slog.Logger) Option
    ```

- **Params:** `logger *slog.Logger` — the logger `DynamoDB` should use.
- **Behavior:** `Option` is a functional option for `NewDynamoDB`; `WithLogger` overrides the default logger.

### `NewDynamoDB`

??? note "Signature"

    ```go
    func NewDynamoDB(ctx context.Context, region, table string, opts ...Option) (*DynamoDB, error)
    ```

- **Params:** `ctx context.Context`; `region string`; `table string`; `opts ...Option`.
- **Behavior:** loads the default AWS config for `region` and wires a `dynamodb.Client` as `dynamoAPI`; `now` defaults to `time.Now` and `logger` to `slog.Default()`, both overridable via `Option`.

### Method behavior (DynamoDB)

??? note "`Get`"

    - `GetItem` with `ConsistentRead: true` (provisioning decisions are made from this read, so a stale replica is not acceptable).
    - Returns `ErrNotFound` when the item is absent.

??? note "`Create`"

    - Validates the record, defaults `Version` to 1, then `PutItem` with `ConditionExpression: attribute_not_exists(#id)`.
    - A condition failure maps to `ErrAlreadyExists`.

??? note "`UpdatePhase`"

    - Rejects the transition client-side via `core.ValidateTransition` before any network call.
    - Then `UpdateItem` with `ConditionExpression: attribute_exists(#id) AND #version = :current AND #phase = :from` — asserting **both** phase and version, so a racing writer that already advanced the record loses this write instead of silently overwriting it.
    - Uses `ReturnValuesOnConditionCheckFailure: ReturnValuesOnConditionCheckFailureAllOld` to distinguish not-found from version-conflict without a second read: an empty returned item means `ErrNotFound`, a non-empty one means `ErrVersionConflict`.

??? note "`Touch`"

    - `UpdateItem` setting `LastReportedAt` with only `attribute_exists(#id)` as its condition.
    - **Invariant:** deliberately no version check, so frequent heartbeats never contend with a phase transition in progress.

??? note "`RecordOIDCIssuer`"

    - Same no-version-check pattern as `Touch`, sets `OIDCIssuer` once.

??? note "`RecordFindings`"

    - Same no-version-check pattern, sets `Findings` and `FindingsAt` together, replacing whatever was recorded before.

??? note "`List`"

    - Delegates to `queryIndex` when `filter.Provider` is set (queries the `ProviderPhaseIndex` GSI, optionally further filtered by `Phase` in the key condition), otherwise to `scan` (a full `Scan`, optionally filtered by `Phase` via `FilterExpression`).
    - Both paginate via `LastEvaluatedKey`/`ExclusiveStartKey` until exhausted.

??? note "`AcquireLease`"

    - `UpdateItem` setting `LeaseHolder`/`LeaseExpiresAt` with condition `attribute_exists(#id) AND (attribute_not_exists(#holder) OR #expires < :now OR #holder = :holder)` — succeeds when the lease is free, expired, or already owned by `holder`.
    - A condition failure with a non-empty returned item and a set holder maps to `ErrLeaseHeld` (and is logged at Warn); an empty item maps to `ErrNotFound`.

??? note "`RenewLease`"

    - Condition `attribute_exists(#id) AND #holder = :holder AND #expires > :now` — strictly greater than now, so an already-expired lease cannot be renewed (another holder may already own it).
    - Failure maps to `ErrLeaseLost` (or `ErrNotFound` if the cluster itself is gone).

??? note "`ReleaseLease`"

    - `REMOVE #holder, #expires` with condition `attribute_exists(#id) AND #holder = :holder`.
    - Failure maps to `ErrLeaseLost`/`ErrNotFound` the same way.

### Helper functions

??? note "`leaseConflict`"

    ```go
    func leaseConflict(id, item, sentinel) error
    ```

    - **Behavior:** distinguishes "no such cluster" (`ErrNotFound`, empty item) from a genuine lease conflict (`sentinel`, wrapped with the current holder if present).

??? note "`conditionFailed` / `conditionFailure`"

    ```go
    func conditionFailed(err) bool
    func conditionFailure(err) (map[string]types.AttributeValue, bool)
    ```

    - **Behavior:** unwrap a `*types.ConditionalCheckFailedException` and surface the item DynamoDB returns alongside the failed condition.

## memory.go

### `Memory`

??? abstract "Signature"

    ```go
    type Memory struct {
        mu      sync.Mutex
        records map[core.ClusterID]Record
        now     func() time.Time
    }
    ```

- **Purpose:** an in-memory `Registry`, so every component built on the registry — the orchestrator above all — is testable without credentials or a container.
- **Invariant:** explicitly documented as not a simplified stand-in — it enforces exactly the same conditions as `DynamoDB`, and both implementations are exercised by the same contract test suite, so a fake with weaker semantics can't let real bugs pass.
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

Mirrors `DynamoDB` exactly, implemented against the map instead of conditional `UpdateItem` calls:

??? note "`Create`"

    - `ErrAlreadyExists` if the key exists; defaults `Version` to 1.

??? note "`UpdatePhase`"

    - Validates the transition via `core.ValidateTransition` first (failing before touching stored state, not persisting and cleaning up after).
    - Then compares both `stored.Version != rec.Version` and `stored.Phase != rec.Phase` before mutating — `ErrVersionConflict` otherwise.
    - On success bumps `Version`, sets `Phase`, and stamps `UpdatedAt` from the injected clock.

??? note "`Touch` / `RecordOIDCIssuer` / `RecordFindings`"

    - Mutate directly with no version bump, matching the "not a phase transition" reasoning in `dynamo.go`.

??? note "`List`"

    - Filters in-memory by `Provider`/`Phase`, then sorts by `ClusterID` for deterministic output (DynamoDB has no equivalent ordering guarantee, but tests need one).

??? note "`AcquireLease`"

    - `ErrLeaseHeld` if `stored.Lease` is non-nil, unexpired, and held by a different holder; otherwise overwrites `stored.Lease`.

??? note "`RenewLease`"

    - `ErrLeaseLost` if there is no lease, it's held by someone else, or it has already expired.

??? note "`ReleaseLease`"

    - `ErrLeaseLost` if there is no lease or it's held by someone else; otherwise clears `stored.Lease`.
