# Execution Plan: turning the implementation plan into code

**Companion to:** [IMPLEMENTATION-PLAN-multicloud-k8s-platform-cli.md](IMPLEMENTATION-PLAN-multicloud-k8s-platform-cli.md)

The milestone plan says *what* to build and in what order. This document says *how to start on Monday*: the decisions that must be locked before the first commit, the PR-sized units for the milestones that are actually next (M0–M2), and the sequencing changes I'd make to the rest. Depth is deliberately front-loaded — detailed task breakdowns for M5+ written today would be fiction, because M2's interfaces will reshape them.

---

## 0. Decisions to lock before the first commit

These are cheap now and expensive at M4. Where I've picked a default, it's a recommendation, not a discovery — override freely.

| Decision | Recommendation | Why |
|---|---|---|
| Module path | `github.com/<org>/kubespin` | **Needs your org.** Blocks `go mod init`; everything else waits on it. |
| Binary name | `kubespin` | The plan writes `mycli` as a placeholder. Kill the placeholder now — it will otherwise end up in docs and CI. |
| Go version | 1.26 (local toolchain is go1.26.5) | Pin in `go.mod` + CI + `.tool-versions` so all three agree. |
| CLI | `spf13/cobra` + `spf13/viper` | Per plan. Precedence: flags > `KUBESPIN_*` env > config file > profile defaults. Wire this once in M0; retrofitting precedence later breaks tests. |
| Cloud SDKs | `aws-sdk-go-v2`, `cloud.google.com/go/container/apiv1`, `Azure/azure-sdk-for-go/.../armcontainerservice` | Per plan. |
| Git host client | `google/go-github` | Per plan. **Needs confirmation:** GitHub Enterprise Server (custom base URL) or github.com/Enterprise Cloud? Changes client construction and rate-limit math in M8. |
| Fleet infra provisioning | `aws-sdk-go-v2` converge loop in `internal/fleetinfra`, run by `kubespin fleet bootstrap` | Go only: no second toolchain, no state file, and the stack is unit-testable against fakes like everything else. Trades away `plan`/rollback; bought back with a mandatory account guard, a real `--dry-run`, no delete path, and a tested no-op-on-rerun property. |
| Test strategy | Interfaces + in-memory fakes for unit tests; real cloud/DynamoDB behind `//go:build integration` | Keeps `go test ./...` fast and credential-free; integration runs are opt-in and CI-gated. |
| CI | GitHub Actions: lint, unit test, build, plus a nightly integration job | `golangci-lint` is not installed locally either — add it to the Makefile bootstrap target. |
| Error handling | Wrapped sentinel errors (`errors.Is/As`), no `panic` outside `main` | The orchestrator (§3) branches on error identity to decide resume vs abort. |

**Assumptions I'm proceeding under** (flag if wrong): one AWS account dedicated to fleet infra, separate from every cluster account; per-cluster repos named `cluster-<ClusterID>` in a single GitHub org; `platform-profiles` lives in that same org.

---

## 1. One structural addition to the plan

The milestone plan names every component but not the thing that sequences them. `apply` has to drive: acquire lease → create cluster → bind identity → create/seed repo → install Argo CD → mark ready, writing the registry phase after each step.

Make that an explicit package — `internal/orchestrator` — introduced in M1 (with all steps stubbed) rather than growing organically inside `cmd/apply.go`. Two properties fall out of it that are painful to add later:

- **Resumability.** A failed `apply` leaves a cluster mid-phase. The orchestrator reads the current phase from the registry and re-enters at that step, so retry is the same code path as first run — this is what makes the M3 idempotency acceptance criteria achievable rather than a special case.
- **Testability.** With every step behind an interface, the full `apply` state machine is unit-testable against fakes with zero cloud credentials, well before any real provisioner exists.

## 2. One sequencing change

The plan builds all three clouds in parallel across M2 (weeks 3–7). I'd insert a one-week interface-hardening step first:

1. Define `ClusterProvisioner` / `IdentityProvisioner` and implement **AWS only**, end to end.
2. Let that implementation reshape the interfaces (it will — most likely around async operation handling and how `Reconcile` reports "nothing to do").
3. *Then* fan out GCP and Azure in parallel against a proven shape.

The M2 gate stays exactly as written — nothing addon-related starts until all three clouds pass. This only reduces the odds of two teams building against an interface that changes underneath them in week 5.

---

## 3. M0 — Foundations, as five PRs

**M0.1 — Repo skeleton and toolchain**
`go.mod`, `Makefile` (`build`, `test`, `lint`, `bootstrap`, `integration`), `.golangci.yml`, `.tool-versions`, `LICENSE`, real `README.md`.
*Done when:* `make build test lint` passes on a clean checkout.

**M0.2 — Shared domain types** (`internal/core/`)
`ClusterID`, `ClusterSpec` (provider, region, `Access: private|public`, node pools, profile ref), `Profile`, `AddonRef`, `Phase` + its legal-transition table. Validation methods on each. Pure types — no I/O, no cloud imports, no dependencies on any other internal package. This package is imported by everything, so keeping it dependency-free prevents the import cycles that otherwise show up around M3.
*Done when:* table-driven tests cover `ClusterSpec` validation and every legal/illegal phase transition.

**M0.3 — CLI skeleton** (`cmd/`)
Root command with global flags (`--config`, `--log-level`, `--dry-run`), viper precedence wired and tested, structured logging (`log/slog`), `--version` via ldflags. Stubs: `apply`, `delete`, `fleet update`, `fleet audit`, `fleet status`.
*Done when:* `kubespin --help` lists all commands; a test asserts flag > env > file precedence.

**M0.4 — Fleet infra bootstrap** (`infra/fleet/`)
SDK converge steps for the DynamoDB table (see M1.1 for schema), API Gateway + Go Lambda skeleton returning 501, and least-privilege IAM, driven by `kubespin fleet bootstrap`.
*Done when:* `kubespin fleet bootstrap` in the fleet account produces a reachable empty table and a live (stub) endpoint, **and a second `--dry-run` reports everything in sync**.

**M0.5 — CI**
Lint + unit test + build on PR; artifact upload on main; nightly integration job with cloud creds via OIDC (no long-lived keys — the same identity discipline the product enforces).
*Done when:* CI is green and required for merge.

> M0 gate (from the plan): `kubespin --help` runs, shared types compile, CI green, empty Fleet Registry table exists and is reachable.

---

## 4. M1 — Fleet Registry, as four PRs

**M1.1 — Table schema.** PK `ClusterID`. Attributes: `Phase`, `Provider`, `Region`, `ProfileRef`, `Access`, `Version` (optimistic-concurrency counter), `LastReportedAt`, `LeaseHolder`, `LeaseExpiresAt`, `CreatedAt`, `UpdatedAt`. Add a GSI on `Provider`+`Phase` now — M8's `fleet audit`/`fleet update` need to enumerate by provider, and adding a GSI to a large table later is a slow online operation.

**M1.2 — Registry client** (`internal/registry/`). Interface (`Get`, `Put`, `UpdatePhase`, `List`, `Touch`) plus a DynamoDB implementation. Every write carries a `ConditionExpression` on `Version`; `UpdatePhase` rejects illegal transitions using M0.2's table, so an invalid state machine move fails at the storage boundary rather than being silently persisted.

**M1.3 — Lease primitive.** `AcquireLease(ClusterID, holder, ttl)` / `Renew` / `Release`, implemented as a conditional write on `attribute_not_exists(LeaseHolder) OR LeaseExpiresAt < :now`. TTL-based so a crashed `apply` self-heals instead of wedging a cluster forever. Long operations renew in the background.

**M1.4 — Fakes and integration tests.** An in-memory `Registry` fake for all downstream unit tests, plus integration tests against DynamoDB Local in Docker behind the `integration` build tag.

> M1 gate (from the plan): create/read/update/lock a fake cluster record end to end; a concurrent-write test proves the lock rejects a second in-flight `apply`. Test that second case with real goroutine contention, not a sequential simulation — sequential tests pass against a broken lock.

Ship `internal/orchestrator` alongside M1.4 with every step a no-op stub, so the state machine is under test before any provisioner exists.

---

## 5. M2 — Provisioning

**M2.1** Define `ClusterProvisioner` (`Create`/`Describe`/`Reconcile`/`Delete`) and `IdentityProvisioner` (`ProvisionForComponent`). Three shape questions to settle here, because they're what the AWS spike in §2 is meant to answer:

- **Async.** EKS/GKE/AKS creation takes 10–30 minutes. Does `Create` block, or return a handle that the orchestrator polls? Polling composes better with resumability and with lease renewal — a blocking call that outlives its lease is a bug generator.
- **`Reconcile` no-op signalling.** It must distinguish "changed something" from "already correct" as data, not by comparing before/after states — M3's acceptance criteria depend on proving a true no-op.
- **Access mode.** `private` vs `public` is a `Create`-time argument on all three clouds; keep it in the shared interface, never as a cloud-specific option bag.

**M2.2** AWS: EKS via `aws-sdk-go-v2`, endpoint config branching on access mode; OIDC provider + IRSA role/policy in the identity impl.
**M2.3 / M2.4** GCP (GKE + Workload Identity) and Azure (AKS + federated credential/managed identity), built in parallel once the interface is proven.
**M2.5** Egress allowlist rule for `fleet-status-reporter`'s destination, provisioned per-cloud inside `Create`. Easy to defer and painful to backfill — every cluster built without it needs a network change at M6.

> M2 gate (from the plan): `apply --provider {aws,gcp,azure}` × `--access {private,public}` — six combinations, each a real cluster in a real non-prod account, reachable only as designed, with correctly bound identity. Hard gate; nothing addon-related starts until it passes.

---

## 6. M3–M10 — deltas from the source plan

The plan's task lists hold up; these are the points where I'd do something differently or where a decision is currently missing.

- **M3 (repo + reconciliation).** The infra-diff/addon-diff split is the load-bearing piece. Make `.state.yaml` a canonicalized hash of resolved desired state (stable key ordering, normalized defaults) — the no-op criterion fails on serialization noise otherwise. Decide the repo naming convention here, before repos exist.
- **M4 (profile catalog).** Specify the override-patch semantics explicitly (strategic merge? JSON Patch? plain deep-merge with list-replace?). "Applied without duplicating the base" is a merge-semantics claim, and the ambiguity resolves as a bug at M7 when `tier-regulated` layers a strict policy set.
- **M5 (Argo CD).** The plan already flags per-cluster Argo CD overhead as a risk — measure actual CPU/memory here and treat "too heavy for `tier-small`" as a real branch, not a footnote. The public/private-aware ingress defaults and the Kyverno public-exposure-deny rule must be tested *together*: a misconfigured default that the policy then blocks is the correct outcome and worth an explicit test.
- **M6 (status reporter).** Design signature verification against replay from day one — the acceptance criterion "cluster A cannot spoof cluster B" is about audience/subject binding in the token, and per-cloud identity mechanisms differ enough that this needs three separate verification paths in one Lambda.
- **M8 (fleet ops).** The plan says design worker pools and backoff "from M8 onward" — put the rate-limited client in `internal/repo` at **M3** instead. Every GitHub call in the codebase should go through it from the first call, not be retrofitted once 50+ clusters exist.
- **M9 (decommission).** Orphan detection needs to be an actual check, not an eyeball: a post-delete verification pass that queries each cloud for resources tagged with the `ClusterID` and fails if any remain.
- **M10 (load test).** Registry-only load (1,000+ entries, mocked cloud/git) validates the fan-out path but not GitHub's real rate limiter. Pair it with a smaller real-API run to calibrate the mock's assumptions.

---

## 7. Open questions

Blocking M0:

1. **Module path / GitHub org** — needed for `go mod init`.
2. **GitHub Enterprise Server or Cloud?** — changes `go-github` construction and M8 rate-limit budgets.
3. **AWS fleet account ID + region** for the registry table and ingestion API.

Answerable later, but cheaper decided now: cluster repo naming convention (M3), profile override-patch semantics (M4), and whether `platform-profiles` shares the org with cluster repos.

---

## Suggested first step

M0.1–M0.3 are self-contained and unblocked except for the module path. Say the word and I'll scaffold them — module, Makefile, linter config, domain types with tests, and the cobra/viper skeleton with precedence tests — as three reviewable commits.
