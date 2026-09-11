# AGENTS.md

Orientation for AI coding agents working in this repository.
Mirrors [CLAUDE.md](CLAUDE.md) in content but is structured for agents:
read **both** files before making substantive changes.

---

## Build / test / lint (must-pass gate)

```bash
go build ./...          # all packages must compile
go test ./...           # -race -cover; all tests must pass
golangci-lint run       # zero lint errors
make docs               # must be a no-op if no cobra tree changed
```

`make bootstrap` installs `golangci-lint` if absent.  
`make lambda` cross-compiles `cmd/ingestion` for `linux/arm64` (required before `fleet bootstrap`).

---

## Repository invariants — read before changing anything

These are hard constraints. A design that violates one is wrong, not the
invariant.

### Outbound-only
Nothing on the fleet-management side may reach into a cluster. Status arrives
via push (`fleet-status-reporter` CronJob → Central Ingestion API). An agent
must never suggest polling a cluster endpoint or an in-cluster webhook.

### Fleet Registry ownership
- One table: `fleet_registry` in a Postgres database the operator supplies via
  `KUBESPIN_REGISTRY_DSN`. Never a CLI flag — connection strings carry passwords.
- Every read/write goes through `internal/registry.Registry` — no raw SQL
  outside that package.
- `internal/registry.Postgres` self-migrates its schema on first connect (via
  `schemaDDL` / `argoCDDetailsDDL`). **Exception:** the ingestion Lambda calls
  `NewPostgres` with `WithoutMigration()` to avoid DDL lock contention on cold
  starts. The CLI owns migrations; Lambda does not.
- Connection-pool defaults inside `NewPostgres`: 25 open / 10 idle / 15 min
  lifetime. The Lambda overrides these with `WithConnectionPool(2, 1, 15m)`.
  Adjust with `WithConnectionPool` — never call `db.SetMax*` directly outside
  `postgres.go`.

### Idempotent apply (split-diff)
`apply` clones the cluster repo, hashes desired state against `.state.yaml`,
then routes changes:
- Infra diff → cloud SDK calls
- Addon diff → git commit + push

A no-change `apply` must produce **zero commits** and **zero cloud calls**.

### Nil-content = file deletion in `Provisioner.Push`
`Push(ctx, checkout, files, msg)` interprets a `nil` value in `files` as "delete
this path from the Git tree." This is the only mechanism for removing files from
a cluster repo. `ReconcileAppOfApps` uses it to purge Application manifests for
addons that were removed from the size tier. Both `githubProvisioner` and
`Memory` honour this convention — do not add special-case deletion APIs.

### Three clouds, shared interfaces
`ClusterProvisioner`, `IdentityProvisioner`, `NetworkProvisioner` — one
implementation per cloud under `internal/provisioner/{aws,gcp,azure}`. No
cloud conditionals outside those directories.

### No second toolchain
Fleet infrastructure is provisioned by `internal/fleetinfra` via AWS SDK —
no Terraform, no CloudFormation, no shell scripts. Every step is
create-or-update; `Plan` is strictly read-only.

---

## Package layout (quick cheat-sheet)

```
cmd/kubespin/                   thin main(); delegates to internal/cli
cmd/ingestion/                  Lambda handler; WithoutMigration + WithConnectionPool(2,1,15m)
cmd/fleet-status-reporter/      in-cluster CronJob; Argo CD → signed push
internal/cli/                   cobra command tree
internal/core/                  shared domain types (dependency-free leaf)
internal/auth/                  cloud auth: aws/gcloud/az shell integration
internal/registry/              Fleet Registry client + lease
internal/orchestrator/          per-cluster phase state machine (apply/delete)
internal/provisioner/{aws,gcp,azure}  ClusterProvisioner + IdentityProvisioner + NetworkProvisioner
internal/repo/                  GitHub repo CRUD; Push nil-content = delete
internal/catalog/               size resolution (small/medium/large, builtin) + override merge
internal/argocd/                app-of-apps manifests, Helm install
internal/fleet/                 fleet-wide audit, update, status
internal/fleetinfra/            SDK converge engine for the fleet infra (Lambda/IAM/API GW)
internal/ingestion/             token verification + registry write path
internal/reporter/              Argo CD summary + signed push (in-cluster)
internal/version/               build metadata (-ldflags)
internal/tools/docsgen/         regenerates docs/cli/*.md from cobra tree
```

---

## Code conventions

### Error wrapping
All errors returned from non-trivial operations must be wrapped with context:
```go
return fmt.Errorf("doing the thing: %w", err)   // ✓
return err                                         // ✗ — loses call-site context
```
Recent improvements wrapped bare `return err` in `scanRecord`, `findingsJSON`,
`deleteVPC`, `deleteNetwork`, `drainLoadBalancers`, and `waitForArgoCDEndpoint`.
Follow the same pattern.

### Deferred resource close
Use the blank-identifier pattern to silence the linter on errors that cannot be
meaningfully handled in a defer:
```go
defer func() { _ = rows.Close() }()   // ✓
defer rows.Close()                     // ✗ — golangci-lint warns
```

### Functional options pattern
`internal/registry.Postgres` and `internal/repo.githubProvisioner` use
`func(*T)` functional options. Add new configurable behaviours as new `Option`
functions — do not add constructor parameters.

### Registry options applied after pool defaults
`NewPostgres` sets pool defaults *before* applying `opts`, so a
`WithConnectionPool` call in opts can override them. Maintain this ordering if
you touch `NewPostgres`.

---

## Testing patterns

| Layer | Test approach |
|---|---|
| `internal/registry` | Contract test suite (`contract_test.go`) runs against `Memory` always, against `Postgres` under `-tags integration`. Both must pass the same set of assertions. |
| `internal/repo` | `fakeGitHub` / `Memory` provisioner. `fakeGitHub.CreateTree` must honour nil `SHA`+`Content` as a deletion. |
| `internal/orchestrator`, `internal/fleet` | Use `registry.NewMemory()` and `repo.NewMemory()`. |
| Cloud provisioners | Narrow interface fakes per cloud; no AWS/GCP/Azure credentials needed for unit tests. |
| `internal/fleetinfra` | Fake per-service interfaces; `Plan` vs `Converge` split keeps read-only paths testable without credentials. |

When adding a new `Provisioner.Push` call-site that deletes files, write a
table-driven test against `repo.NewMemory()` asserting the path is absent after
the call — like `TestReconcileAppOfApps_RemovesDeletedAddon`.

---

## Safe vs. unsafe operations for agents

### Safe to do without extra review
- Adding `Option` funcs to `postgres.go` or `repo/provisioner.go`
- Adding or modifying unit/integration tests
- Regenerating `docs/cli/*.md` via `make docs`
- Updating `docs/reference/*.md` to match code changes
- Fixing error wrapping (`fmt.Errorf("…: %w", err)`)
- Adding entries to `CLAUDE.md` / `AGENTS.md`

### Requires extra care
- **Schema changes in `schemaDDL` / `argoCDDetailsDDL`** — must be written
  with `IF NOT EXISTS` / `IF EXISTS` so they are safe to run on an already-
  migrated database. Never drop a column without an `IF EXISTS` guard.
- **`core.Phase` or `forwardTransitions` changes** — update `PhaseOrder` and
  all callers; run the full contract test suite.
- **`Provisioner` interface changes** — both `githubProvisioner` and `Memory`
  must satisfy the interface (static assertions exist); update both.
- **`Registry` interface changes** — both `Postgres` and `Memory` must satisfy
  the interface; the contract test suite must pass against both.
- **`make docs`** — run it and commit the result any time cobra commands/flags
  change; a PR that changes the cobra tree but not `docs/cli/` is incomplete.

### Never do
- Add a CLI flag for `KUBESPIN_REGISTRY_DSN` (password leaks into shell history)
- Skip the nil-as-deletion convention — do not add a separate `DeleteFile` API
- Call `db.SetMax*` outside `NewPostgres` in `postgres.go`
- Add cloud-specific conditionals outside `internal/provisioner/{aws,gcp,azure}`
- Add a second toolchain (Terraform, Helm CLI, kubectl, etc.)

---

## Docs that must stay in sync

When you change code, update the corresponding reference doc:

| Code change | Doc to update |
|---|---|
| `internal/registry/postgres.go` | `docs/reference/registry.md` |
| `internal/repo/` | `docs/reference/repo.md` |
| `cmd/ingestion/main.go` | `docs/reference/entrypoints.md` |
| `cmd/fleet-status-reporter/` | `docs/reference/entrypoints.md` |
| `internal/core/` | `docs/reference/core.md` |
| `internal/orchestrator/` | `docs/reference/orchestrator.md` |
| `internal/provisioner/` | `docs/reference/provisioner-{aws,gcp,azure}.md` |
| `internal/fleetinfra/` | `docs/reference/fleetinfra.md` |
| `internal/ingestion/` | `docs/reference/ingestion.md` |
| cobra command tree | run `make docs`; commit result |
| Architecture invariants | `CLAUDE.md`, `AGENTS.md` (this file) |
