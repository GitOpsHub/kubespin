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

---

## Repository invariants — read before changing anything

These are hard constraints. A design that violates one is wrong, not the
invariant.

### No control plane, no agent
kubespin is a CLI that runs on an operator's machine. There is no always-on
kubespin service, no central Argo CD hub, and nothing kubespin-owned running
inside a provisioned cluster. Every cluster-touching operation is a direct,
operator-initiated connection from wherever the CLI runs — which is why
`--access private` requires that machine to already have reachability into
the cluster's VPC/VNet. An agent must never propose an in-cluster agent, a
scheduled push, or a service that watches clusters between commands.

### Cluster registry ownership
- One table: `fleet_registry` in a Postgres database the operator supplies via
  `KUBESPIN_REGISTRY_DSN`. Never a CLI flag — connection strings carry passwords.
  (The table name is a fossil of the removed fleet functionality; renaming it
  would break live deployments, so it stays.)
- It is a resume log for `apply`/`delete` plus a lease, one row per cluster —
  not an inventory, and never a source of liveness. Nothing reports into it.
- Every read/write goes through `internal/registry.Registry` — no raw SQL
  outside that package.
- `internal/registry.Postgres` self-migrates its schema on first connect (via
  `schemaDDL` / `argoCDDetailsDDL`).
- Connection-pool defaults inside `NewPostgres`: 25 open / 10 idle / 15 min
  lifetime. Adjust with `WithConnectionPool` — never call `db.SetMax*` directly
  outside `postgres.go`.

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
`ClusterProvisioner` (`Create`/`Describe`/`Reconcile`/`Delete`) and
`NetworkProvisioner` (`EnsureNetwork`/`DeleteNetwork`) — one implementation
per cloud under `internal/provisioner/{aws,gcp,azure}`. No cloud conditionals
outside those directories.

### No second toolchain
Every cloud resource is provisioned by `internal/provisioner/{aws,gcp,azure}`
through each cloud's Go SDK — no Terraform, no CloudFormation, no shell
scripts, no `helm`/`kubectl` binaries. Every step is create-or-update, and a
dry run is strictly read-only.

---

## Package layout (quick cheat-sheet)

```
cmd/kubespin/                   thin main(); delegates to internal/cli
internal/cli/                   cobra command tree
internal/core/                  shared domain types (dependency-free leaf)
internal/auth/                  cloud auth: aws/gcloud/az shell integration
internal/registry/              cluster registry client + lease
internal/orchestrator/          per-cluster phase state machine (apply/delete)
internal/provisioner/{aws,gcp,azure}  ClusterProvisioner + NetworkProvisioner
internal/repo/                  GitHub repo CRUD; Push nil-content = delete
internal/catalog/               size resolution (small/medium/large, builtin) + override merge
internal/argocd/                app-of-apps manifests, Helm install
internal/kubeconfig/            operator kubeconfig update after apply (shells out to aws/gcloud/az)
internal/tools/changeloggen/    derives CHANGELOG.md and next SemVer from Conventional Commits
internal/tools/docsgen/         regenerates docs/cli/*.md from cobra tree
internal/version/               build metadata (-ldflags)
```

---

## Code conventions

### Error wrapping
All errors returned from non-trivial operations must be wrapped with context:
```go
return fmt.Errorf("doing the thing: %w", err)   // ✓
return err                                         // ✗ — loses call-site context
```
Recent improvements wrapped bare `return err` in `scanRecord`, `deleteVPC`,
`deleteNetwork`, `drainLoadBalancers`, and `waitForArgoCDEndpoint`. Follow the
same pattern.

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
| `internal/orchestrator` | Use `registry.NewMemory()` and `repo.NewMemory()`. |
| Cloud provisioners | Narrow interface fakes per cloud; no AWS/GCP/Azure credentials needed for unit tests. |

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
| `cmd/kubespin/main.go` | `docs/reference/entrypoints.md` |
| `internal/core/` | `docs/reference/core.md` |
| `internal/orchestrator/` | `docs/reference/orchestrator.md` |
| `internal/provisioner/` | `docs/reference/provisioner-{aws,gcp,azure}.md` |
| `internal/kubeconfig/` | `docs/reference/kubeconfig.md` |
| cobra command tree | run `make docs`; commit result |
| Architecture invariants | `CLAUDE.md`, `AGENTS.md` (this file) |
