# Implementation Plan: Multi-Cloud Kubernetes Platform CLI

**Companion to:** ADR-001 (Accepted, Finalized)
**Scope:** Go CLI + local Argo CD spoke model + enterprise addon catalog, supporting private and public clusters across EKS/GKE/AKS built in parallel, GitHub-hosted per-cluster repos, AWS-hosted fleet infra.

This plan sequences the ADR's four phases into concrete, dependency-ordered engineering work with acceptance criteria per milestone. Each milestone is a gate — don't start the next until the current one's acceptance criteria pass on all three clouds.

---

## Milestone 0 — Foundations & Scaffolding (Week 1–2)

**Goal:** repo, tooling, and shared types exist before any cloud-specific code.

- [ ] Go module scaffold: `cmd/` (cobra CLI entrypoints), `internal/provisioner/{aws,gcp,azure}`, `internal/identity/`, `internal/repo/`, `internal/registry/`, `internal/catalog/`
- [ ] Shared domain types: `ClusterID`, `ClusterSpec` (incl. new `Access: private|public` field), `Profile`, `AddonRef`
- [ ] `cobra` + `viper` CLI skeleton: `apply`, `delete`, `fleet update`, `fleet audit`, `fleet status` as stub commands
- [ ] CI pipeline for the CLI itself (lint, test, build binary artifact)
- [ ] AWS account/project bootstrap for shared platform infra (separate from any cluster account) — DynamoDB table, API Gateway + Lambda skeleton for Fleet Registry

**Acceptance criteria:** `mycli --help` runs, shared types compile, CI green, empty Fleet Registry table exists in AWS and is reachable.

---

## Milestone 1 — Fleet Registry & State Machine (Week 2–3)

**Goal:** the durable backbone every other component writes to/reads from exists before provisioning logic depends on it.

- [ ] DynamoDB schema: partition key `ClusterID`, attributes for state machine phase (`pending → cluster-created → identity-bound → repo-pushed → argocd-installed → ready`), metadata (provider, region, profile, access mode), and a `LastReportedAt` field for staleness detection
- [ ] Distributed lock primitive (conditional writes / DynamoDB lease) keyed by `ClusterID`, used to prevent concurrent `apply` races
- [ ] Registry client library used by every downstream component (`internal/registry`)

**Acceptance criteria:** can create/read/update/lock a fake cluster record end-to-end from a test; concurrent-write test proves the lock rejects a second in-flight `apply`.

---

## Milestone 2 — Cluster & Identity Provisioning, All Three Clouds in Parallel (Week 3–7)

Build these as three parallel workstreams against one shared interface — this is where "all three clouds in parallel" actually happens.

- [ ] `ClusterProvisioner` interface: `Create`, `Describe`, `Reconcile` (node pools/sizing), `Delete`
  - **AWS:** `aws-sdk-go-v2` EKS client; handle `Access: public` (API server authorized CIDRs) vs `Access: private` (no public endpoint) at creation time
  - **GCP:** `container/apiv1` GKE client; same public/private branching via authorized networks / private cluster config
  - **Azure:** `armcontainerservice` AKS client; same via API server allowed IP ranges / private cluster flag
- [ ] `IdentityProvisioner` interface: `ProvisionForComponent`
  - **AWS:** OIDC provider setup + IRSA role/policy creation
  - **GCP:** Workload Identity binding
  - **Azure:** OIDC issuer + federated credential + managed identity
- [ ] Network provisioning step: egress allowlist rule for `fleet-status-reporter`'s destination (Central Ingestion API domain), provisioned per-cloud as part of `Create`

**Acceptance criteria:** `mycli apply --provider {aws,gcp,azure} --access private` and `--access public` each produce a running, reachable-only-as-designed cluster with correctly bound identity, verified against a real (non-prod) account per cloud. This is the first hard gate — nothing addon-related starts until this passes on all three.

---

## Milestone 3 — Per-Cluster GitHub Repo & Idempotent Reconciliation (Week 5–7, overlaps M2)

- [ ] `RepoProvisioner` interface via GitHub Enterprise API (`go-github`): `Exists`, `Create`, `Clone`, `Push`
- [ ] Repo seeding logic: resolve profile (`tier-small` to start) → render `cluster.yaml`, `addons.yaml`, `.state.yaml`
- [ ] Branch protection + CODEOWNERS templating applied at repo creation
- [ ] Idempotent diff logic: on repeat `apply`, clone → hash current desired state → compare to `.state.yaml` → split into infra-diff (cloud SDK call) vs addon-diff (commit + push only)

**Acceptance criteria:** first `apply` creates and seeds a repo; second `apply` with no changes is a true no-op (no commits, no cloud calls); a changed node pool size triggers a cloud reconcile but not a git commit, and vice versa for an addon value change.

---

## Milestone 4 — Profile Catalog & `platform-profiles` Repo (Week 6–7)

- [ ] Stand up `platform-profiles` repo (AWS-hosted org, same GitHub Enterprise)
- [ ] Define `tier-small` profile: CNI (cloud default / Cilium), cert-manager, Gateway API impl (per-cloud), ESO, Kyverno (baseline), Cluster Autoscaler, kube-prometheus-stack, Fluent Bit, OpenCost, ExternalDNS, fleet-status-reporter
- [ ] Profile resolution logic in the CLI: profile + per-cluster override patch → resolved `addons.yaml`

**Acceptance criteria:** a cluster's `addons.yaml` correctly resolves to the full `tier-small` set with a test override patch applied without duplicating the base.

---

## Milestone 5 — Local Argo CD Bootstrap & Addon Delivery (Week 7–9)

**Goal:** first real addons land on a cluster with zero manual `kubectl`/`helm` commands.

- [ ] Argo CD Helm-as-library install (`helm.sh/helm/v3/pkg/action`), self-referential — points at the cluster's own repo `/addons.yaml`-resolved manifests
- [ ] App-of-apps pattern: one root Application discovers per-addon Applications (each addon = independent Application, independent sync/failure)
- [ ] Ingress/Gateway addon templated with public/private-aware defaults (internal LB unless `access: public` + `ingress.exposure: external`)
- [ ] Kyverno baseline policy shipped as an addon, including the public-exposure-deny rule

**Acceptance criteria:** running `mycli apply` on a fresh cluster results in all `tier-small` addons `Synced`/`Healthy` in Argo CD with no manual steps, on all three clouds, in both public and private access modes.

---

## Milestone 6 — Fleet-Status-Reporter & Central Ingestion (Week 8–10, overlaps M5)

**Goal:** close the loop from Section 6/drift design — near-real-time status without any inbound access.

- [ ] `fleet-status-reporter` CronJob: queries local Argo CD API, builds compact status payload, signs with cloud-native identity (IRSA/WI/MI), pushes every 2–3 min
- [ ] Central Ingestion API (API Gateway + Lambda): verifies signature per cloud's identity mechanism, writes to Fleet Registry
- [ ] Staleness detection: Fleet Registry flags a cluster stale after N missed intervals

**Acceptance criteria:** killing network access to a cluster's local Argo CD does not affect status reporting availability elsewhere; a genuinely unreachable cluster is flagged stale within the expected window; a signature from cluster A cannot be replayed to spoof cluster B's status.

---

## Milestone 7 — Enterprise Hardening: `tier-standard` / `tier-regulated` (Week 10–12)

- [ ] `tier-standard` profile adds: Velero, Falco, Argo CD (already present as bootstrap, now tracked as catalog entry too), Karpenter (EKS)
- [ ] `tier-regulated` profile adds: strict Kyverno set (deny privileged pods, mandatory quotas, mandatory network policy, image-signature verification, public-exposure-deny), audit logging, OTel tracing

**Acceptance criteria:** a `tier-regulated` cluster fails admission for a test manifest violating each policy; Velero backup/restore verified on a real PVC; Falco alert verified against a test syscall trigger.

---

## Milestone 8 — Fleet-Wide Operations (Week 12–14)

- [ ] `fleet update --profile X --component Y --version Z`: rate-limited worker pool, patches every matching cluster's repo, staged in waves (canary tier first)
- [ ] `fleet audit`: scheduled job, cloud-SDK describe calls per cluster, diffs live infra vs `cluster.yaml`, writes findings to Fleet Registry
- [ ] Fleet dashboard (reads Fleet Registry): sync status, drift findings, staleness, correlated by cluster ID and commit SHA

**Acceptance criteria:** a simulated fleet of 50+ registry entries survives a `fleet update` wave without exceeding GitHub API rate limits; `fleet audit` correctly flags a manually-resized node pool as drifted within one run.

---

## Milestone 9 — Decommissioning & Lifecycle Completeness (Week 13–14, overlaps M8)

- [ ] `mycli delete`: reverse teardown — mark decommissioning in registry → IAM/OIDC cleanup → cluster delete → repo archive (not delete) → registry status `decommissioned`
- [ ] Verified against all three clouds and both access modes

**Acceptance criteria:** no orphaned IAM roles, OIDC providers, or cloud resources remain after delete; repo is archived with full history intact.

---

## Milestone 10 — Load Test & Production Readiness (Week 14–16)

- [ ] Synthetic load test: simulate 1,000+ Fleet Registry entries, run `fleet audit` and a `fleet update` wave against them (mocked cloud/git calls where real infra isn't practical at that scale)
- [ ] Runbook: on-call procedure for stale clusters, failed `apply` retries, Central Ingestion API outages
- [ ] Onboard first 2–3 real teams on `tier-small`/`tier-standard` as a pilot before general rollout

**Acceptance criteria:** load test completes within acceptable time/rate-limit bounds; pilot teams successfully self-serve a cluster with zero platform-team manual intervention.

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
