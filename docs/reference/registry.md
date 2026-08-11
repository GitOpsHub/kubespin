# internal/registry

`internal/registry` is the client for the Fleet Registry — the single source
of durable fleet state, keyed by `ClusterID`. Every other component
(`internal/orchestrator`, `internal/fleet`, `internal/cli`) reads and writes
cluster state exclusively through this package's `Registry` interface rather
than issuing raw DynamoDB SDK calls, so the invariants that keep the fleet
consistent live in one place: an illegal phase transition (`pending →
cluster-created → identity-bound → repo-pushed → argocd-installed → ready`) is
rejected before it reaches storage, a read-modify-write against a stale
`Version` is rejected instead of silently overwritten, and a lease held by
another holder cannot be taken, renewed, or released out from under them. The
lease itself is a time-bounded claim (`Lease{Holder, ExpiresAt}`) stored on
the same record: `AcquireLease` performs a DynamoDB conditional `UpdateItem`
that only succeeds when the lease is unset, already owned by the caller, or
expired, which is what serializes concurrent `apply` runs against the same
cluster while letting a crashed run self-heal once its TTL passes rather than
wedging the cluster forever.

## Types (`registry.go`)

### `Lease`

Purpose: a time-bounded claim on a cluster, preventing two concurrent `apply`
runs from provisioning the same cluster at once.

```go
type Lease struct {
	Holder    string
	ExpiresAt time.Time
}

func (l Lease) Expired(now time.Time) bool
```

Invariants: expires rather than being held indefinitely — `Expired` is the
sole check used both client-side (`Record.Held`) and to distinguish a stale
lease from a live conflict during acquisition.

### `Record`

Purpose: one cluster's row in the registry — the durable half of a cluster's
state. The other half (resolved addons, node pool detail) lives in the
cluster's own repository; `Record` holds only what the fleet needs to reason
about centrally.

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

Fields:

- `OIDCIssuer` — recorded once identity binding (M2) succeeds; the Central
  Ingestion API verifies `fleet-status-reporter`'s signature against exactly
  this issuer, which is what makes a signature from one cluster unusable to
  spoof another.
- `Version` — bumped on every data write and asserted as a condition, so a
  racing read-modify-write fails instead of overwriting.
- `Findings` / `FindingsAt` — the drift `fleet audit` last found. An empty,
  non-nil slice with a non-zero `FindingsAt` means the cluster was audited and
  found clean; a zero `FindingsAt` means never audited. Audit is the only
  writer — `apply`/`delete` never touch this field.
- `Lease` — nil when unheld; an expired lease may still be present until the
  next acquisition overwrites it.

Methods:

- `Stale(now, threshold)` — true only for a `PhaseReady` cluster that has
  missed its reporting window, judged from `LastReportedAt` (or `CreatedAt` if
  it has never reported). A statement about missing reports, never about
  reachability — nothing in this package connects to a cluster.
- `Held(now)` — true when `Lease` is non-nil and unexpired at `now`.
- `Validate()` — checks `ClusterID`, `Phase.Valid()`, `Provider.Valid()`,
  non-empty `Region`, and `Access.Valid()`, joining all failures with
  `errors.Join` and wrapping each in `core.ErrInvalidSpec`.

Functions:

- `NewRecord(spec core.ClusterSpec, now time.Time) Record` — builds a
  `PhasePending` record from a validated `ClusterSpec`, `Version` seeded at 1.

### `Filter`

Purpose: narrows a `List` call. A zero `Filter` matches every cluster.

```go
type Filter struct {
	Provider core.Provider
	Phase    core.Phase
}
```

Invariant: setting `Provider` routes `DynamoDB.List` through the
`ProviderPhaseIndex` GSI instead of a table scan — the index exists from the
day the table does specifically so fleet-wide operations can filter by
provider cheaply.

### `Registry` (interface)

Purpose: the durable store of fleet state — the contract both `DynamoDB` and
`Memory` implement identically.

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

Invariants implementations must enforce (callers rely on these rather than
re-checking):

- `UpdatePhase` rejects an illegal transition with `ErrInvalidTransition`
  (checked against `core.ValidateTransition`), and rejects a write against a
  stale `Phase`/`Version` pair with `ErrVersionConflict`.
- `Touch`, `RecordOIDCIssuer`, and `RecordFindings` carry **no** version
  check — heartbeats, identity-issuer recording, and audit findings are
  metadata writes that must not contend with an in-flight phase transition.
- `AcquireLease` fails with `ErrLeaseHeld` if another holder's lease is still
  valid; an expired lease is taken over without ceremony.
- `RenewLease` fails with `ErrLeaseLost` if the caller's lease already
  expired — silently re-acquiring here would defeat the lock, since another
  holder may already own it.
- `ReleaseLease` fails with `ErrLeaseLost` if the lease is held by someone
  else.

### Sentinel errors

```go
var (
	ErrNotFound        = errors.New("cluster not found in registry")
	ErrAlreadyExists    = errors.New("cluster already exists in registry")
	ErrVersionConflict  = errors.New("registry record was modified concurrently")
	ErrLeaseHeld        = errors.New("cluster lease is held by another holder")
	ErrLeaseLost        = errors.New("cluster lease is no longer held by this holder")
)
```

Callers branch with `errors.Is` rather than matching messages.

## `DynamoDB` (`dynamo.go`)

Purpose: the production `Registry`, backed by the Fleet Registry table
created by `fleet bootstrap`.

```go
type DynamoDB struct {
	client dynamoAPI
	table  string
	now    func() time.Time
	logger *slog.Logger
}

type Option func(*DynamoDB)

func WithLogger(logger *slog.Logger) Option
func NewDynamoDB(ctx context.Context, region, table string, opts ...Option) (*DynamoDB, error)
```

- `dynamoAPI` is a narrow interface (`GetItem`, `PutItem`, `UpdateItem`,
  `Query`, `Scan`) declared in this package, not the full DynamoDB client, so
  the registry is testable without credentials and its permission surface is
  explicit.
- `NewDynamoDB` loads the default AWS config for `region` and wires a
  `dynamodb.Client` as `dynamoAPI`; `now` defaults to `time.Now` and `logger`
  to `slog.Default()`, both overridable via `Option`.
- Registry logging is Debug-level diagnostic detail except a lease conflict,
  logged at Warn — that is the exact race the lease exists to catch.

Method behavior:

- **`Get`** — `GetItem` with `ConsistentRead: true` (provisioning decisions
  are made from this read, so a stale replica is not acceptable). Returns
  `ErrNotFound` when the item is absent.
- **`Create`** — validates the record, defaults `Version` to 1, then
  `PutItem` with `ConditionExpression: attribute_not_exists(#id)`. A
  condition failure maps to `ErrAlreadyExists`.
- **`UpdatePhase`** — rejects the transition client-side via
  `core.ValidateTransition` before any network call, then `UpdateItem` with
  `ConditionExpression: attribute_exists(#id) AND #version = :current AND
  #phase = :from` — asserting **both** phase and version, so a racing writer
  that already advanced the record loses this write instead of silently
  overwriting it. Uses `ReturnValuesOnConditionCheckFailure:
  ReturnValuesOnConditionCheckFailureAllOld` to distinguish not-found from
  version-conflict without a second read: an empty returned item means
  `ErrNotFound`, a non-empty one means `ErrVersionConflict`.
- **`Touch`** — `UpdateItem` setting `LastReportedAt` with only
  `attribute_exists(#id)` as its condition — deliberately no version check, so
  frequent heartbeats never contend with a phase transition in progress.
- **`RecordOIDCIssuer`** — same no-version-check pattern as `Touch`, sets
  `OIDCIssuer` once.
- **`RecordFindings`** — same no-version-check pattern, sets `Findings` and
  `FindingsAt` together, replacing whatever was recorded before.
- **`List`** — delegates to `queryIndex` when `filter.Provider` is set
  (queries the `ProviderPhaseIndex` GSI, optionally further filtered by
  `Phase` in the key condition), otherwise to `scan` (a full `Scan`,
  optionally filtered by `Phase` via `FilterExpression`). Both paginate via
  `LastEvaluatedKey`/`ExclusiveStartKey` until exhausted.
- **`AcquireLease`** — `UpdateItem` setting `LeaseHolder`/`LeaseExpiresAt`
  with condition `attribute_exists(#id) AND (attribute_not_exists(#holder) OR
  #expires < :now OR #holder = :holder)` — succeeds when the lease is free,
  expired, or already owned by `holder`. A condition failure with a non-empty
  returned item and a set holder maps to `ErrLeaseHeld` (and is logged at
  Warn); an empty item maps to `ErrNotFound`.
- **`RenewLease`** — condition `attribute_exists(#id) AND #holder = :holder
  AND #expires > :now` — strictly greater than now, so an already-expired
  lease cannot be renewed (another holder may already own it). Failure maps to
  `ErrLeaseLost` (or `ErrNotFound` if the cluster itself is gone).
- **`ReleaseLease`** — `REMOVE #holder, #expires` with condition
  `attribute_exists(#id) AND #holder = :holder`. Failure maps to
  `ErrLeaseLost`/`ErrNotFound` the same way.

Helper functions:

- `leaseConflict(id, item, sentinel) error` — distinguishes "no such cluster"
  (`ErrNotFound`, empty item) from a genuine lease conflict (`sentinel`,
  wrapped with the current holder if present).
- `conditionFailed(err) bool` / `conditionFailure(err) (map[string]types.AttributeValue, bool)`
  — unwrap a `*types.ConditionalCheckFailedException` and surface the item
  DynamoDB returns alongside the failed condition.

## Marshaling (`item.go`)

Not exported, but load-bearing for the wire format:

- Attribute name constants (`attrClusterID`, `attrPhase`, `attrLeaseHolder`,
  `attrLeaseExpiresAt`, etc.) — only `ClusterID`, `Provider`, and `Phase` are
  part of the table's key schema or the `ProviderPhaseIndex` GSI; the rest are
  schemaless.
- `epochMillis(t time.Time) string` / `fromEpochMillis(s string) (time.Time, error)`
  — lease expiry is stored as epoch milliseconds rather than an RFC3339
  string specifically so the `AcquireLease` condition can compare it with `<`
  numerically; RFC3339 strings are variable-width and would not order
  correctly under a string comparison.
- `marshalRecord(rec Record) map[string]types.AttributeValue` /
  `unmarshalRecord(item map[string]types.AttributeValue) (Record, error)` —
  convert between `Record` and the DynamoDB item shape. Optional fields
  (`LastReportedAt`, `OIDCIssuer`, `Lease`, `Findings`/`FindingsAt`) are
  written **absent**, not as empty/zero values, when unset — e.g. "never
  reported" and "reported at the zero time" must not collapse into the same
  item, and an absent `FindingsAt` (never audited) must stay distinguishable
  from an empty `Findings` list (audited and clean).
- `stringAttr` / `numberAttr` — typed attribute readers used throughout
  `unmarshalRecord`.
- `key(id core.ClusterID) map[string]types.AttributeValue` — builds the
  primary-key item for `GetItem`/`UpdateItem` calls.

## `Memory` (`memory.go`)

Purpose: an in-memory `Registry`, so every component built on the registry —
the orchestrator above all — is testable without credentials or a container.
Explicitly documented as not a simplified stand-in: it enforces exactly the
same conditions as `DynamoDB`, and both implementations are exercised by the
same contract test suite, so a fake with weaker semantics can't let real bugs
pass.

```go
type Memory struct {
	mu      sync.Mutex
	records map[core.ClusterID]Record
	now     func() time.Time
}

type MemoryOption func(*Memory)

func WithClock(now func() time.Time) MemoryOption
func NewMemory(opts ...MemoryOption) *Memory
```

- `WithClock` replaces the time source so lease expiry is testable without
  sleeping.
- `NewMemory` returns an empty registry guarded by a single `sync.Mutex`;
  every method takes the lock for its full body.
- `clone(rec Record) Record` — deep-copies a record (specifically the `Lease`
  pointer) before returning it, so callers cannot mutate stored state through
  a pointer they were handed.

Method behavior mirrors `DynamoDB` exactly, implemented against the map
instead of conditional `UpdateItem` calls:

- `Create` — `ErrAlreadyExists` if the key exists; defaults `Version` to 1.
- `UpdatePhase` — validates the transition via `core.ValidateTransition`
  first (failing before touching stored state, not persisting and cleaning up
  after), then compares both `stored.Version != rec.Version` and
  `stored.Phase != rec.Phase` before mutating — `ErrVersionConflict`
  otherwise. On success bumps `Version`, sets `Phase`, and stamps `UpdatedAt`
  from the injected clock.
- `Touch` / `RecordOIDCIssuer` / `RecordFindings` — mutate directly with no
  version bump, matching the "not a phase transition" reasoning in
  `dynamo.go`.
- `List` — filters in-memory by `Provider`/`Phase`, then sorts by
  `ClusterID` for deterministic output (DynamoDB has no equivalent ordering
  guarantee, but tests need one).
- `AcquireLease` — `ErrLeaseHeld` if `stored.Lease` is non-nil, unexpired, and
  held by a different holder; otherwise overwrites `stored.Lease`.
- `RenewLease` — `ErrLeaseLost` if there is no lease, it's held by someone
  else, or it has already expired.
- `ReleaseLease` — `ErrLeaseLost` if there is no lease or it's held by
  someone else; otherwise clears `stored.Lease`.
