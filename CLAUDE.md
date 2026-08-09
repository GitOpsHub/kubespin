# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository state

kubespin is **greenfield** — as of this writing the repo contains only [IMPLEMENTATION-PLAN-multicloud-k8s-platform-cli.md](IMPLEMENTATION-PLAN-multicloud-k8s-platform-cli.md), a README stub, and a Go `.gitignore`. There is no `go.mod`, no source, no CI, and no build/test/lint commands yet. Milestone 0 of the plan creates them.

When adding the first code, follow the plan's layout rather than inventing a new one, and update this file with the real build/test commands once they exist (`go build ./...`, `go test ./...`, `golangci-lint run`, plus whatever CI actually runs).

## What is being built

A Go CLI (cobra + viper) that provisions and manages Kubernetes clusters across EKS, GKE, and AKS, in both private and public access modes. Each cluster gets its own GitHub repo and its own **local** Argo CD instance that syncs from that repo — there is no central Argo CD hub and **no inbound network access to any cluster**. State flows the other direction: an in-cluster `fleet-status-reporter` CronJob pushes signed status to a central ingestion API.

Read the implementation plan before making architectural decisions; it is a companion to ADR-001 (not in this repo) and is the authoritative source for scope, sequencing, and acceptance criteria.

## Architecture invariants

These constrain nearly every design decision — violating one usually means the design is wrong, not that the invariant should bend:

- **Outbound-only.** Nothing on the fleet-management side may require reaching into a cluster. Status, drift, and health information arrives via push from inside the cluster.
- **Fleet Registry (DynamoDB) is the single source of durable fleet state.** Keyed by `ClusterID`, tracking a phase state machine: `pending → cluster-created → identity-bound → repo-pushed → argocd-installed → ready`. Every component reads/writes it through `internal/registry`, never with raw SDK calls. Concurrent `apply` races are prevented by a DynamoDB conditional-write lease keyed on `ClusterID`.
- **Cloud-native workload identity everywhere** — IRSA (AWS), Workload Identity (GCP), federated credential + managed identity (Azure). No static credentials in clusters; the status reporter's signature is what proves a cluster's identity to the ingestion API, so signatures must not be replayable across clusters.
- **`apply` is idempotent and split-diff.** Clone the cluster repo, hash desired state against `.state.yaml`, then route changes: infra diffs become cloud SDK calls, addon diffs become a git commit + push. A no-change `apply` must produce zero commits and zero cloud calls.
- **The three clouds are built against shared interfaces, in parallel** — `ClusterProvisioner` (`Create`/`Describe`/`Reconcile`/`Delete`) and `IdentityProvisioner` (`ProvisionForComponent`). Cloud-specific behavior belongs behind these interfaces in `internal/provisioner/{aws,gcp,azure}`; no cloud conditionals should leak into command or catalog code.
- **`Access: private|public` is a first-class field on `ClusterSpec`**, not an afterthought. It branches cluster creation (endpoint/authorized-networks config per cloud) *and* addon templating (internal LB unless `access: public` and `ingress.exposure: external`), and is enforced at admission by a Kyverno public-exposure-deny policy.
- **Go only — no second toolchain.** Fleet infrastructure is provisioned by `internal/fleetinfra` through `aws-sdk-go-v2`, not Terraform or CloudFormation. There is no state file, so convergence is the contract: every step is create-or-update, never delete, `Plan` is strictly read-only (that is what makes `--dry-run` honest), and a run against provisioned infrastructure must report no changes. Each AWS service is reached through a narrow interface declared in the package so the whole engine is testable without credentials.
- **Delete is a reverse teardown, and repos are archived, not deleted:** mark decommissioning → IAM/OIDC cleanup → cluster delete → repo archive → registry `decommissioned`.

## Planned package layout

```
cmd/kubespin/                 cobra entrypoints: apply, delete, fleet bootstrap|update|audit|status
cmd/ingestion/                Central Ingestion API handler (Go, deployed to Lambda)
internal/fleetinfra/          SDK converge engine behind `fleet bootstrap`
internal/provisioner/{aws,gcp,azure}   ClusterProvisioner impls (EKS/GKE/AKS)
internal/identity/            IdentityProvisioner impls (IRSA / Workload Identity / federated credential)
internal/repo/                RepoProvisioner over GitHub Enterprise (go-github): Exists/Create/Clone/Push
internal/registry/            Fleet Registry client + lease/locking
internal/catalog/             profile resolution (tier-small/standard/regulated + per-cluster override patches)
```

Shared domain types (`ClusterID`, `ClusterSpec`, `Profile`, `AddonRef`) are defined once and consumed by all of the above.

## Cluster repo contract

Each provisioned cluster's GitHub repo holds three files, and their roles must stay distinct:

- `cluster.yaml` — desired infra (provider, region, access mode, node pools). Drives cloud SDK calls; `fleet audit` diffs live infra against it.
- `addons.yaml` — resolved addon set (profile + override patch, flattened without duplicating the base). Argo CD syncs from this.
- `.state.yaml` — last-applied hash used for idempotent diffing. Not user-authored.

Addons are delivered app-of-apps: one root Argo CD Application discovers one Application per addon, so addons sync and fail independently. Argo CD itself is installed via the Helm Go library (`helm.sh/helm/v3/pkg/action`), never by shelling out to `helm` or `kubectl`.

## Working with the plan

Milestones are gates — the plan explicitly says not to start the next until the current one's acceptance criteria pass **on all three clouds**. M2 (cluster + identity provisioning) is the hard gate before any addon work. When completing work, check off the relevant boxes in the plan rather than tracking status elsewhere.
