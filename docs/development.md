# Development guide

## Toolchain

Go 1.26, pinned in `go.mod`, `.tool-versions`, and CI. `golangci-lint` is the
only tool not vendored through `go.mod`:

```bash
make bootstrap
```

Then the default target — lint, test, build:

```bash
make
```

| Target | What it does |
|---|---|
| `make build` | Builds `bin/kubespin`, and `bin/ingestion/bootstrap` as a dependency |
| `make lambda` | Builds only the ingestion handler: Linux arm64, static |
| `make test` | Unit tests with `-race -cover` |
| `make integration` | Adds `-tags=integration`; needs credentials or DynamoDB Local |
| `make lint` | `golangci-lint run` |
| `make docs` | Regenerates `docs/cli` from the command tree |
| `make fmt` | `go fmt` plus `go mod tidy` |

## Layout

```
cmd/kubespin/          binary entrypoint; wires signals and exit codes
cmd/ingestion/         Central Ingestion API handler, deployed to Lambda
internal/cli/          cobra command tree and configuration resolution
internal/core/         shared domain types
internal/registry/     Fleet Registry client, lease primitive, in-memory implementation
internal/orchestrator/ sequences one cluster's provisioning through the phases
internal/fleetinfra/   SDK converge engine behind `fleet bootstrap`
internal/tools/        build-time tools (docs generation)
internal/version/      build metadata stamped in via -ldflags
docs/cli/              generated — never edit by hand
```

**`internal/core` imports nothing from `internal/`.** No cloud SDKs, no I/O, no
other internal package. Everything imports core, so keeping it a leaf is what
prevents the import cycles that otherwise appear as the tree grows. If you find
yourself wanting to import something into core, the type probably belongs
elsewhere.

## Error conventions

Sentinel errors, wrapped, matched with `errors.Is`/`errors.As` — never by string
comparison. Each package exposes its own: `core.ErrInvalidSpec`,
`core.ErrInvalidTransition`, `cli.ErrConfig`, `cli.ErrNotImplemented`,
`fleetinfra.ErrSpec`, `fleetinfra.ErrAccountMismatch`.

`wrapcheck` is enabled, so any error crossing a package boundary must be
wrapped with context.

Validation functions return **all** problems at once via `errors.Join`. Fixing a
spec one error per run is miserable; see `ClusterSpec.Validate` in
[internal/core/cluster.go](../internal/core/cluster.go).

## Testing

Unit tests run without credentials and without network. Anything needing real
AWS or DynamoDB Local goes behind the build tag:

```go
//go:build integration
```

Those run via `make integration` and nightly in CI, never on a pull request.

To run the registry integration tests locally:

```bash
docker run -d --rm -p 8000:8000 --name kubespin-ddb-local amazon/dynamodb-local:latest
```

```bash
KUBESPIN_DYNAMODB_ENDPOINT=http://localhost:8000 make integration
```

Note that `make test` alone reports low coverage for `internal/registry`: the
DynamoDB implementation is only reachable under the integration tag. The number
is not a measure of how well that package is tested.

### The registry contract

`internal/registry/contract_test.go` is the behaviour every implementation must
satisfy, written once and run against both the in-memory registry and DynamoDB
Local. Add new registry behaviour there rather than in an implementation's own
test file — a guarantee proven against only one implementation is not a
guarantee.

The in-memory registry deliberately enforces the same conditions as DynamoDB.
It is not a simplified stand-in: if it accepted writes DynamoDB would reject,
the orchestrator tests built on it would pass while production failed.

The load-bearing case is `concurrent acquisition elects exactly one holder`,
which races sixteen goroutines at a single lease. It runs against real DynamoDB
too, because that is the only place the conditional-write expression itself is
under test. A sequential simulation of this would pass against a broken lock.

### Testing against AWS

[internal/fleetinfra/fake_test.go](../internal/fleetinfra/fake_test.go) holds
`fakeAWS`, an in-memory stand-in implementing all six service interfaces. It
records every call by name, which lets tests assert *which calls were made*, not
just the resulting state. Two helpers matter:

- `assertNoMutations(t)` fails if any state-changing call was made. This is what
  keeps `--dry-run` honest — the guarantee is enforced by a test, not by careful
  reading.
- `provisioned(t)` returns a fake that has already been fully converged, and is
  the starting point for every drift test.

The load-bearing test is `TestConverge_SecondRunIsNoOp`. Without a state file,
convergence is only trustworthy if a run against provisioned infrastructure
reports nothing *and* calls nothing. Every drift case additionally converges a
third time to prove the repair itself settles.

The phase transition test asserts the **full cartesian product** of phases
against a hand-written table rather than a rule shared with the implementation.
That is what caught the original `Phase.Valid()` bug, where validity was derived
from having a successor and `ready` therefore reported itself invalid.

## Adding a converge step

Steps live in `internal/fleetinfra/step_*.go` and satisfy:

```go
type step interface {
    Name() string
    Plan(ctx context.Context) (Action, error)
    Apply(ctx context.Context, a Action) error
}
```

1. **Add only the calls you need** to the relevant narrow interface in
   [clients.go](../internal/fleetinfra/clients.go). These interfaces double as
   the documented permission set for operators, so an unused method there is a
   permission someone will grant for no reason.
2. **Keep `Plan` strictly read-only.** It is what `--dry-run` executes. Store
   what you discovered on the step struct for `Apply` to consume.
3. **Set `action.Resource` to `s.Name()`**, not the AWS resource name — the role,
   function, and API all resolve to the same AWS name, and identically labelled
   report lines are ambiguous to both readers and tests.
4. **Populate `action.Details`** with what specifically differs. It is printed on
   dry and real runs alike and is the first thing someone debugging drift reads.
5. **Never delete.** Create-or-update only.
6. **Register it** in `Converge` in dependency order.
7. **Add a drift case** to `TestConverge_DetectsDrift`: a mutation, the step name,
   and the repairing call. The shared harness then checks that a dry run detects
   it without repairing, a real run repairs it, and a third run is clean.

Also add the new mutating call names to `mutatingCalls` in `fake_test.go`, or
`assertNoMutations` will silently stop covering them.

## Changing the CLI

`docs/cli` is generated by `internal/tools/docsgen`. After adding or changing a
command or flag:

```bash
make docs
```

CI regenerates and fails on any difference, so a stale reference blocks the
merge rather than misleading a reader. The generator disables cobra's
auto-generated date tag and blanks the version string — both would otherwise
churn the output on every run.

New commands that are not implemented yet should return `ErrNotImplemented` via
the `stub` helper in [internal/cli/root.go](../internal/cli/root.go). They exit
3: failing loudly beats exiting 0 and implying work was done.

Global flags belong in `registerGlobalFlags`, which takes a `*pflag.FlagSet`
rather than a command so the precedence tests can exercise it without building
the whole tree. If you add one, extend `Config` and its validation, and remember
the precedence contract: **flags > `KUBESPIN_*` env > config file > defaults**.
`TestLoadConfig_Precedence` covers every pairing, including the boolean case that
viper's flag binding is easiest to get wrong on.

## CI

`.github/workflows/ci.yml` runs on every pull request:

- **lint** — golangci-lint, plus checks that `go mod tidy` and `make docs` were
  run and committed
- **test** — race-enabled unit tests with coverage
- **build** — linux/amd64, linux/arm64, darwin/arm64

`.github/workflows/integration.yml` runs nightly against DynamoDB Local, and
against real AWS via OIDC once `AWS_INTEGRATION_ROLE_ARN` is configured. There
are no long-lived cloud credentials in this repository — the same identity
discipline the product enforces on clusters.
