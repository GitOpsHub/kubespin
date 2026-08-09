# Implementation Plan: Multi-Cloud Kubernetes Platform CLI

**Companion to:** ADR-001 (Accepted, Finalized)
**Scope:** Go CLI + local Argo CD spoke model + enterprise addon catalog, supporting private and public clusters across EKS/GKE/AKS built in parallel, GitHub-hosted per-cluster repos, AWS-hosted fleet infra.

This plan sequences the ADR's four phases into concrete, dependency-ordered engineering work with acceptance criteria per milestone. Each milestone is a gate — don't start the next until the current one's acceptance criteria pass on all three clouds.

---

## Milestone 0 — Foundations & Scaffolding (Week 1–2)

**Goal:** repo, tooling, and shared types exist before any cloud-specific code.

- [~] Go module scaffold: `cmd/` (cobra CLI entrypoints), `internal/provisioner/{aws,gcp,azure}`, `internal/identity/`, `internal/repo/`, `internal/registry/`, `internal/catalog/`
  - `cmd/`, `internal/registry/`, and `internal/orchestrator/` exist. `provisioner`, `identity`, `repo`, and `catalog` are created by the milestones that fill them (M2–M4) rather than as empty directories.
- [x] Shared domain types: `ClusterID`, `ClusterSpec` (incl. new `Access: private|public` field), `Profile`, `AddonRef`
- [x] `cobra` + `viper` CLI skeleton: `apply`, `delete`, `fleet update`, `fleet audit`, `fleet status` as stub commands
- [x] CI pipeline for the CLI itself (lint, test, build binary artifact)
- [~] AWS account/project bootstrap for shared platform infra (separate from any cluster account) — DynamoDB table, API Gateway + Lambda skeleton for Fleet Registry
  - Implemented as `kubespin fleet bootstrap` (Go + AWS SDK, no Terraform). **Not yet applied** — blocked on the fleet account ID and region.

**Acceptance criteria:** `mycli --help` runs, shared types compile, CI green, empty Fleet Registry table exists in AWS and is reachable.

---

## Milestone 1 — Fleet Registry & State Machine (Week 2–3)

**Goal:** the durable backbone every other component writes to/reads from exists before provisioning logic depends on it.

- [x] DynamoDB schema: partition key `ClusterID`, attributes for state machine phase (`pending → cluster-created → identity-bound → repo-pushed → argocd-installed → ready`), metadata (provider, region, profile, access mode), and a `LastReportedAt` field for staleness detection
- [x] Distributed lock primitive (conditional writes / DynamoDB lease) keyed by `ClusterID`, used to prevent concurrent `apply` races
- [x] Registry client library used by every downstream component (`internal/registry`)

**Acceptance criteria:** can create/read/update/lock a fake cluster record end-to-end from a test; concurrent-write test proves the lock rejects a second in-flight `apply`.

---

## Milestone 2 — Cluster & Identity Provisioning, All Three Clouds in Parallel (Week 3–7)

Build these as three parallel workstreams against one shared interface — this is where "all three clouds in parallel" actually happens.

- [x] `ClusterProvisioner` interface: `Create`, `Describe`, `Reconcile` (node pools/sizing), `Delete`
  - Interface defined; **AWS, GCP, and Azure implemented**, each behind `internal/provisioner/{aws,gcp,azure}` and unit-tested against a fake of its cloud SDK.
  - **AWS:** `aws-sdk-go-v2` EKS client; handle `Access: public` (API server authorized CIDRs) vs `Access: private` (no public endpoint) at creation time
  - **GCP:** `container/apiv1` GKE client; same public/private branching via `PrivateClusterConfig` / `MasterAuthorizedNetworksConfig`
  - **Azure:** `armcontainerservice` AKS client; same via `APIServerAccessProfile`'s `EnablePrivateCluster` / `AuthorizedIPRanges`
- [x] `IdentityProvisioner` interface: `ProvisionForComponent`
  - Interface defined; **AWS (OIDC + IRSA), GCP (Workload Identity), and Azure (OIDC issuer + federated credential + user-assigned managed identity) implemented.**
  - **AWS:** OIDC provider setup + IRSA role/policy creation
  - **GCP:** Workload Identity binding (service account + `roles/iam.workloadIdentityUser` binding scoped to one KSA)
  - **Azure:** OIDC issuer + federated identity credential scoped to one KSA + managed identity
- [x] Network provisioning step: egress allowlist rule for `fleet-status-reporter`'s destination (Central Ingestion API domain), provisioned per-cloud as part of `Create`
  - **AWS** (cluster security group egress rule), **GCP** (VPC firewall egress rule), **Azure** (NSG egress rule in the AKS node resource group) all implemented.

**Acceptance criteria:** `mycli apply --provider {aws,gcp,azure} --access private` and `--access public` each produce a running, reachable-only-as-designed cluster with correctly bound identity, verified against a real (non-prod) account per cloud. **Not yet met** — the three provisioners are implemented and unit-tested against fakes, but none has been run against a real cloud account. This is the first hard gate — nothing addon-related starts until this passes on all three against real accounts.

---

## Milestone 3 — Per-Cluster GitHub Repo & Idempotent Reconciliation (Week 5–7, overlaps M2)

- [x] `RepoProvisioner` interface via GitHub Enterprise API (`go-github`): `Exists`, `Create`, `Clone`, `Push`
  - Implemented in `internal/repo` (`githubProvisioner`) over the REST + Git Data API (no literal `git clone`/`git push` — see the package doc for why), plus an in-memory `Memory` implementation other packages' tests build on, the way `internal/registry` has `Memory` alongside DynamoDB.
- [x] Repo seeding logic: resolve profile (`tier-small` to start) → render `cluster.yaml`, `addons.yaml`, `.state.yaml`
  - `repo.Render` + `repo.Seed`. Profile resolution goes through the new `internal/catalog` package's `Resolver` interface, currently backed by a single builtin `tier-small` profile — the real platform-profiles-repo-backed resolver is M4 scope.
- [x] Branch protection + CODEOWNERS templating applied at repo creation
  - `githubProvisioner.Create`: requires a CODEOWNERS-reviewed PR (`RequireCodeOwnerReviews`) and seeds a single-team CODEOWNERS file.
- [x] Idempotent diff logic: on repeat `apply`, clone → hash current desired state → compare to `.state.yaml` → split into infra-diff (cloud SDK call) vs addon-diff (commit + push only)
  - `repo.ReconcileAddons` hashes the resolved `addons.yaml` against `.state.yaml`, committing only on a mismatch. Infra drift is left to M2's `ClusterProvisioner.Reconcile`, which already diffs the spec against live cloud state with no hash of its own. Both are now wired into every `apply`, not just the first one: `orchestrator.WithReadyReconcile` runs them whenever a cluster is (or becomes) ready, closing the gap where a repeat `apply` against an already-ready cluster previously ran no steps at all.

**Acceptance criteria:** first `apply` creates and seeds a repo; second `apply` with no changes is a true no-op (no commits, no cloud calls); a changed node pool size triggers a cloud reconcile but not a git commit, and vice versa for an addon value change. **Verified at the package level** (`internal/repo`, `internal/provisioner/*`, `internal/orchestrator` tests, all against fakes) — not yet exercised end-to-end against a real GitHub Enterprise org or a real cloud account.

---

## Milestone 4 — Profile Catalog & `platform-profiles` Repo (Week 6–7)

- [ ] Stand up `platform-profiles` repo (AWS-hosted org, same GitHub Enterprise)
  - The reader side is ready for it: `catalog.RepoResolver` resolves `profiles/<name>/<version>.yaml` out of any repo named by `--profiles-repo`, over the same GitHub clients M3's repo provisioner uses. What remains is standing up the actual org/repo and populating it — a real infra action, not something to script blind.
- [ ] Define `tier-small` profile: CNI (cloud default / Cilium), cert-manager, Gateway API impl (per-cloud), ESO, Kyverno (baseline), Cluster Autoscaler, kube-prometheus-stack, Fluent Bit, OpenCost, ExternalDNS, fleet-status-reporter
  - `internal/catalog` currently ships a two-addon builtin `tier-small` placeholder (cert-manager, fleet-status-reporter) so M3's repo machinery has something real to render; the full addon set still needs defining in the actual repo. `apply` falls back to this builtin catalog whenever `--profiles-repo` is not given, so the CLI stays usable before that repo exists.
- [x] Profile resolution logic in the CLI: profile + per-cluster override patch → resolved `addons.yaml`
  - `core.ClusterSpec.Overrides` (`[]core.AddonOverride`) is the per-cluster patch, authored directly in `cluster.yaml`. `catalog.Merge` applies it onto a resolved `Profile`: patches an addon's version/values in place, can disable one, and errors on an override naming an addon the profile doesn't carry — it never adds or duplicates an entry. Wired into both `internal/orchestrator` steps (`resolveProfile`) so the seeded and reconciled `addons.yaml` both reflect it.

**Acceptance criteria:** a cluster's `addons.yaml` correctly resolves to the full `tier-small` set with a test override patch applied without duplicating the base. **Verified for override merging** (`internal/catalog` and `internal/orchestrator` tests) **and for `platform-profiles`-repo resolution against a fake** (`internal/catalog/repo_resolver_test.go`); the full `tier-small` addon set and a real `platform-profiles` repo are still open.

---

## Milestone 5 — Local Argo CD Bootstrap & Addon Delivery (Week 7–9)

**Goal:** first real addons land on a cluster with zero manual `kubectl`/`helm` commands.

- [~] Argo CD Helm-as-library install (`helm.sh/helm/v3/pkg/action`), self-referential — points at the cluster's own repo `/addons.yaml`-resolved manifests
  - Designed but not implemented: an early attempt wired `helm.sh/helm/v3/pkg/action` directly and it pulled in ~450 lines of new `go.sum` (OCI registry, Prometheus, Redis, SQL drivers — Helm's full transitive tree) for code that cannot be exercised without a live cluster and has no caller yet, since acquiring a `*rest.Config` for a freshly created EKS/GKE/AKS cluster (IAM-signed token / Google OAuth token / Azure AD token, one distinct scheme per cloud) is undesigned. That combination of cost and unverifiability wasn't a good trade, so it was reverted rather than merged half-working. The self-referential part — the root Application pointing at the cluster's own repo — is implemented (`argocd.RenderRootApplication`) and ready for whatever install path lands.
- [x] App-of-apps pattern: one root Application discovers per-addon Applications (each addon = independent Application, independent sync/failure)
  - `internal/argocd`: `RenderRootApplication` (points at `apps/` in the cluster's own repo, never committed there itself) + `RenderAddonApplications` (one manifest per addon under `apps/`, each with its own automated prune/self-heal sync policy). Pure functions, fully unit tested; not yet wired to actually commit `apps/*.yaml` via `repo.Push` or to run the installer above.
- [x] Ingress/Gateway addon templated with public/private-aware defaults (internal LB unless `access: public` + `ingress.exposure: external`)
  - `argocd.ApplyIngressDefaults` / `ApplyProfileIngressDefaults`, wired into `internal/orchestrator`'s `resolveProfile` so both the initial seed (M3) and every subsequent reconcile apply it. `internal/catalog`'s builtin `tier-small` now carries a real `ingress-nginx` addon to exercise it against.
- [x] Kyverno baseline policy shipped as an addon, including the public-exposure-deny rule
  - Added `kyverno` and `kyverno-policies` to the builtin `tier-small` profile, the latter carrying `publicExposureDeny: true`. The rule's actual Kyverno `ClusterPolicy` content still needs authoring in the real platform-profiles repo (M4's still-open item) — this only shapes the addon that will deliver it.

**Acceptance criteria:** running `mycli apply` on a fresh cluster results in all `tier-small` addons `Synced`/`Healthy` in Argo CD with no manual steps, on all three clouds, in both public and private access modes. **Not met** — this needs the installer above and a live cluster; what's landed is the fully-tested manifest-rendering and templating logic everything else depends on.

---

## Milestone 6 — Fleet-Status-Reporter & Central Ingestion (Week 8–10, overlaps M5)

**Goal:** close the loop from Section 6/drift design — near-real-time status without any inbound access.

- [x] `fleet-status-reporter` CronJob: queries local Argo CD API, builds compact status payload, signs with cloud-native identity (IRSA/WI/MI), pushes every 2–3 min
  - `cmd/fleet-status-reporter` + `internal/reporter`. One push per invocation — the Kubernetes CronJob resource owns the 2-3 minute schedule, not a loop inside the binary. `HTTPArgoCDClient` calls Argo CD's real REST API and is tested against an `httptest` server; "signing" is reading the pod's already-cluster-issued projected service account token (`FileTokenSource`) and presenting it as a bearer token — this component mints nothing itself.
- [x] Central Ingestion API (API Gateway + Lambda): verifies signature per cloud's identity mechanism, writes to Fleet Registry
  - `cmd/ingestion` + `internal/ingestion`. `Verifier.Verify` does real JWT/JWKS verification (`github.com/golang-jwt/jwt/v5`) bound to the specific OIDC issuer the Fleet Registry has on file for that `{clusterId}` — not any issuer a shared trust root would accept. `JWKSResolver` does the real OIDC-discovery-then-JWKS network fetch (RSA and EC keys); tests exercise the actual verification path against a real signed JWT via a fixed-key `KeyResolver`, not a mocked-away `Verify`.
  - Registry schema addition: `Record.OIDCIssuer`, written once by `bindIdentityStep` right after M2's identity binding succeeds (`RecordOIDCIssuer`), across both `Memory` and `DynamoDB`.
- [x] Staleness detection: Fleet Registry flags a cluster stale after N missed intervals
  - `Record.Stale` (pre-existing) is now exercised end-to-end: `Touch` is called from the real ingestion handler on every accepted push, and `fleet status --stale-only --stale-threshold` surfaces it.

**Acceptance criteria:** killing network access to a cluster's local Argo CD does not affect status reporting availability elsewhere; a genuinely unreachable cluster is flagged stale within the expected window; a signature from cluster A cannot be replayed to spoof cluster B's status. **The anti-replay property is verified directly**: `TestHandleStatus_ReplayedTokenFromAnotherClusterRejected` signs a genuine token for cluster A and shows the handler rejects it when replayed against cluster B, because verification is bound to B's own recorded issuer. The "genuinely unreachable cluster" and "Argo CD network-partitioned" scenarios need a live multi-cluster fleet to observe, same caveat as every other milestone's live-infra acceptance criteria.

---

## Milestone 7 — Enterprise Hardening: `tier-standard` / `tier-regulated` (Week 10–12)

- [x] `tier-standard` profile adds: Velero, Falco, Argo CD (already present as bootstrap, now tracked as catalog entry too), Karpenter (EKS)
  - `internal/catalog/tiers.go`. Built as tier-small's addon set plus these four, via `withAddons` rather than a second addon list — a real superset, not a parallel definition that could drift from tier-small. Karpenter has no per-provider gate (`core.AddonRef` carries no provider constraint yet), flagged in the code as a real gap for the eventual `platform-profiles` repo to close.
- [x] `tier-regulated` profile adds: strict Kyverno set (deny privileged pods, mandatory quotas, mandatory network policy, image-signature verification, public-exposure-deny), audit logging, OTel tracing
  - Built as tier-standard's set with `kyverno-policies` *replaced* by the strict version (not duplicated — two Kyverno policy Applications would fight over the same `ClusterPolicy` resources) plus `audit-logging` and `otel-collector`.

**Acceptance criteria:** a `tier-regulated` cluster fails admission for a test manifest violating each policy; Velero backup/restore verified on a real PVC; Falco alert verified against a test syscall trigger. **Not met, and not attemptable here** — every one of these needs a live cluster with Kyverno/Velero/Falco actually installed and running (which itself needs M5's still-open Argo CD installer). What's verified instead: both tiers resolve to valid profiles (`core.Profile.Validate`), tier-standard is a genuine superset of tier-small, and tier-regulated's strict policy addon carries every named rule (`internal/catalog/tiers_test.go`).

---

## Milestone 8 — Fleet-Wide Operations (Week 12–14)

- [x] `fleet update --profile X --component Y --version Z`: rate-limited worker pool, patches every matching cluster's repo, staged in waves (canary tier first)
  - `internal/fleet.Update` + `kubespin fleet update`. Reuses `catalog.Merge` and `repo.ReconcileAddons` rather than reimplementing override/commit logic — an update wave is just many clusters each getting one addon override applied on top of whatever they already carry (`updateRecord` reads each cluster's own `cluster.yaml` first specifically so an existing override patch survives the wave). One cluster's failure doesn't abort the run (`UpdateResult` per cluster). **Canary-first staging is not implemented** — every matching cluster updates in the same wave; `--profile` is the only way to scope a wave by hand today.
- [x] `fleet audit`: scheduled job, cloud-SDK describe calls per cluster, diffs live infra vs `cluster.yaml`, writes findings to Fleet Registry
  - `internal/fleet.Audit` + `kubespin fleet audit`. Read-only by design (never calls `Reconcile` or `Push`) — audit surfaces drift for a human or a deliberate `apply` to act on, not to silently correct. **Findings are not written back to the Fleet Registry** — no schema exists for them yet; today `fleet audit` prints them. Adding that is a real schema decision (one finding row per cluster? per finding? how long retained?) deferred rather than guessed at.
- [ ] Fleet dashboard (reads Fleet Registry): sync status, drift findings, staleness, correlated by cluster ID and commit SHA
  - Not built. `fleet status --output json` gives a machine-readable feed a dashboard could consume, but no dashboard (web UI or otherwise) exists.

**Acceptance criteria:** a simulated fleet of 50+ registry entries survives a `fleet update` wave without exceeding GitHub API rate limits; `fleet audit` correctly flags a manually-resized node pool as drifted within one run. **`fleet audit`'s drift detection is verified directly** (`internal/fleet/audit_test.go`: access drift, a resized node pool, and a missing node pool are each independently detected). **Scale and GitHub rate limits are verified against mocks, not real GitHub** — see M10's load test, which runs 1,200 simulated clusters through both `fleet audit` and `fleet update` against an in-memory registry and repo.

---

## Milestone 9 — Decommissioning & Lifecycle Completeness (Week 13–14, overlaps M8)

- [x] `mycli delete`: reverse teardown — mark decommissioning in registry → IAM/OIDC cleanup → cluster delete → repo archive (not delete) → registry status `decommissioned`
  - `Orchestrator.Delete` + `orchestrator.Teardown` + `kubespin delete`. Idempotent and resumable exactly like `apply`: a cluster already decommissioned is a no-op, and a teardown that fails mid-way leaves the registry at `decommissioning` so a retried `delete` resumes rather than needing to be reasoned about by hand (`TestDelete_ResumesAfterAFailedTeardown`). `repo.Provisioner` gained an `Archive` method (GitHub `Edit` with `Archived: true`) alongside `Memory.Archive`, both idempotent. The CLI prompts for the cluster ID to be typed back before proceeding, skippable with `--yes`.
- [ ] Verified against all three clouds and both access modes
  - Not done — same live-account caveat as M2's own acceptance criteria, which this inherits: `Teardown` calls the exact same `IdentityProvisioner.Deprovision` / `ClusterProvisioner.Delete` implementations M2 built and unit-tested per cloud, so there is no new cloud-specific code path to separately verify, but none of it has run against a real account.

**Acceptance criteria:** no orphaned IAM roles, OIDC providers, or cloud resources remain after delete; repo is archived with full history intact. **Not verifiable without a live account** — `Teardown`'s ordering (identity, then cluster, then repo) and idempotence are unit-tested (`internal/orchestrator/delete_test.go`), but "no orphaned resources" is a property of the real cloud APIs' actual behavior, not of this code.

---

## Milestone 10 — Load Test & Production Readiness (Week 14–16)

- [x] Synthetic load test: simulate 1,000+ Fleet Registry entries, run `fleet audit` and a `fleet update` wave against them (mocked cloud/git calls where real infra isn't practical at that scale)
  - `internal/fleet/loadtest_test.go`: 1,200 simulated clusters (exceeding the 1,000+ target) through both `Audit` and `Update`, against an in-memory registry and repo with a synthetic per-call delay so the worker pool's concurrency bound is actually exercised, not just theoretically present — the test asserts the observed max concurrency equals the configured bound (neither exceeding it nor silently running serially).
- [x] Runbook: on-call procedure for stale clusters, failed `apply` retries, Central Ingestion API outages
  - [docs/runbook.md](../docs/runbook.md).
- [ ] Onboard first 2–3 real teams on `tier-small`/`tier-standard` as a pilot before general rollout
  - Not applicable to this pass — onboarding real teams is an organizational rollout step, not something to build or verify in code. It also depends on every open live-infra item above (a real `platform-profiles` repo, a working Argo CD installer, verified teardown) being closed first.

**Acceptance criteria:** load test completes within acceptable time/rate-limit bounds; pilot teams successfully self-serve a cluster with zero platform-team manual intervention. **Load test criterion met**: 1,200 clusters through both operations in well under a second against mocks (real GitHub/cloud API latency and rate limits are a different, live-only concern the load test cannot stand in for). **Pilot criterion not attempted** — see above.

---

## Summary Timeline

| Weeks | Milestones |
|---|---|
| 1–2 | M0 Foundations |
| 2–3 | M1 Fleet Registry |
| 3–7 | M2 Cluster/Identity Provisioning (all 3 clouds) |
| 5–7 | M3 GitHub Repo & Reconciliation |
| 6–7 | M4 Profile Catalog |
| 7–9 | M5 Argo CD Bootstrap & Addons |
| 8–10 | M6 Status Reporter & Ingestion |
| 10–12 | M7 tier-standard / tier-regulated |
| 12–14 | M8 Fleet-Wide Ops |
| 13–14 | M9 Decommissioning |
| 14–16 | M10 Load Test & Pilot |

~16 weeks to a production-ready pilot across all three clouds, both access modes, full enterprise addon catalog. Fleet-wide scale operations (M8) and hardening (M7) are the phases most likely to reveal rework — budget slack there rather than in early foundational milestones.

## Cross-Cutting Risks to Track Throughout

- **GitHub Enterprise / cloud API rate limits** — design worker pools and backoff from M8 onward, don't bolt on later.
- **Ingress-nginx retirement (March 2026)** — if any pilot team has legacy expectations around NGINX annotations, surface this in M5, not at pilot onboarding.
- **Argo CD per-cluster resource overhead at 1,000s of clusters** — validate actual CPU/memory footprint per cluster during M5; if too heavy for small `tier-small` clusters, consider a lighter-weight sync agent for that tier specifically.
- **IAM blast radius** — audit that every `IdentityProvisioner` role is least-privilege before M7 hardening, not after.
