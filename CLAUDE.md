# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository state

Past greenfield — `apply`/`delete`/`fleet bootstrap|update|audit|status`/`login`/`status`/`logout` are all implemented and wired into the CLI, on all three clouds. [IMPLEMENTATION-PLAN-multicloud-k8s-platform-cli.md](IMPLEMENTATION-PLAN-multicloud-k8s-platform-cli.md) is still the authoritative source for scope and milestone sequencing; check its boxes as work lands rather than tracking status elsewhere.

Build/test/lint commands (see [Makefile](Makefile) for the exact targets):

```bash
go build ./...              # or: make build (also builds the ingestion Lambda handler)
go test ./...                # or: make test (-race -cover)
golangci-lint run             # or: make lint
make docs                     # regenerates docs/cli/*.md from the command tree; must be a no-op when current
```

`make bootstrap` installs `golangci-lint` if it isn't already on PATH. `make lambda` cross-compiles the ingestion handler alone (`GOOS=linux GOARCH=arm64`, required before `kubespin fleet bootstrap` — see [docs/fleet-bootstrap.md](docs/fleet-bootstrap.md)).

`make build` also copies the binary onto PATH (`make install`, `INSTALL_DIR` defaulting to `~/.local/bin`), so examples below that call `kubespin` directly work without a `./bin/` prefix. It is skipped when `CI` is set, so runners still build without writing outside the repo tree.

## What is being built

A Go CLI (cobra + viper) that provisions and manages Kubernetes clusters across EKS, GKE, and AKS, in both private and public access modes. Each cluster gets its own GitHub repo and its own **local** Argo CD instance that syncs from that repo — there is no central Argo CD hub and **no inbound network access to any cluster**. State flows the other direction: an in-cluster `fleet-status-reporter` CronJob pushes signed status to a central ingestion API.

Read the implementation plan before making architectural decisions; it is a companion to ADR-001 (not in this repo) and is the authoritative source for scope, sequencing, and acceptance criteria.

## Architecture invariants

These constrain nearly every design decision — violating one usually means the design is wrong, not that the invariant should bend:

- **Outbound-only.** Nothing on the fleet-management side may require reaching into a cluster. Status, drift, and health information arrives via push from inside the cluster.
- **Fleet Registry (DynamoDB) is the single source of durable fleet state.** Keyed by `ClusterID`, tracking a phase state machine: `pending → cluster-created → identity-bound → repo-pushed → argocd-installed → ready`. Every component reads/writes it through `internal/registry`, never with raw SDK calls. Concurrent `apply` races are prevented by a DynamoDB conditional-write lease keyed on `ClusterID`.
- **Cloud-native workload identity everywhere** — IRSA (AWS), Workload Identity (GCP), federated credential + managed identity (Azure). No static credentials in clusters; the status reporter's signature is what proves a cluster's identity to the ingestion API, so signatures must not be replayable across clusters.
- **`apply` is idempotent and split-diff.** Clone the cluster repo, hash desired state against `.state.yaml`, then route changes: infra diffs become cloud SDK calls, addon diffs become a git commit + push. A no-change `apply` must produce zero commits and zero cloud calls.
- **The three clouds are built against shared interfaces, in parallel** — `ClusterProvisioner` (`Create`/`Describe`/`Reconcile`/`Delete`), `IdentityProvisioner` (`ProvisionForComponent`), and `NetworkProvisioner` (`EnsureNetwork`/`AllowEgress`). Cloud-specific behavior belongs behind these interfaces in `internal/provisioner/{aws,gcp,azure}`; no cloud conditionals should leak into command, orchestrator, or catalog code.
- **`--subnets` is optional on every cloud, not just Azure.** When supplied, `EnsureNetwork` passes it through unchanged — kubespin never touches a network an operator already owns. When empty, every provider creates one, deterministically named from the cluster ID so a resumed or repeated `apply` adopts what already exists instead of duplicating it: AWS creates a VPC + 2 subnets across 2 AZs (EKS requires ≥2) + an Internet Gateway + route table; GCP creates a custom-mode VPC network + one regional subnetwork; Azure creates a resource group + VNet + subnet. `orchestrator.createClusterStep` calls `EnsureNetwork` once per apply and feeds the resolved `NetworkResult.SubnetIDs` into `Cluster.Create` — this plumbing is cloud-agnostic, so a fourth provider needs no orchestrator changes.
- **`Access: private|public` is a first-class field on `ClusterSpec`**, not an afterthought. It branches cluster creation (endpoint/authorized-networks config per cloud) *and* addon templating (internal LB unless `access: public` and `ingress.exposure: external`), and is enforced at admission by a Kyverno public-exposure-deny policy.
- **Installing Argo CD is a direct connection, not a push.** `apply` reaches the API server itself (Helm SDK) from whatever machine runs it — nothing inside the cluster pushes the install out. So `--access private` requires the operator's machine to already have network reachability into the cluster's VPC/VNet (VPN, peering, or a bastion), and on GCP, `--access public` alone is not enough either: GKE enables master-authorized-networks with an empty allowlist by default, so `--authorized-cidrs` must include the caller's IP before *anyone*, including the operator, can reach the endpoint. AWS/Azure public endpoints are open to `0.0.0.0/0` unless `--authorized-cidrs` is set. GKE nodes are also always created with `EnablePrivateNodes` regardless of access mode, so a kubespin-managed GCP network provisions a Cloud Router + Cloud NAT alongside the VPC/subnet — without it those nodes have no path to pull an addon's image from a public registry at all.
- **Go only — no second toolchain.** Fleet infrastructure is provisioned by `internal/fleetinfra` through `aws-sdk-go-v2`, not Terraform or CloudFormation. There is no state file, so convergence is the contract: every step is create-or-update, never delete, `Plan` is strictly read-only (that is what makes `--dry-run` honest), and a run against provisioned infrastructure must report no changes. Each AWS service is reached through a narrow interface declared in the package so the whole engine is testable without credentials.
- **Delete is a reverse teardown, and repos are archived, not deleted:** mark decommissioning → IAM/OIDC cleanup → cluster delete → repo archive → registry `decommissioned`.

## Package layout

```
cmd/kubespin/                 main() — delegates entirely to internal/cli.NewRootCommand
cmd/ingestion/                Central Ingestion API handler (Go, deployed to Lambda)
cmd/fleet-status-reporter/    in-cluster CronJob: queries local Argo CD, pushes signed status
internal/cli/                 cobra command tree: apply, delete, fleet bootstrap|update|audit|status, login, status, logout
internal/core/                shared domain types: ClusterID, ClusterSpec, Profile, AddonRef, Access, NodePool
internal/auth/                operator-facing cloud auth: shells out to aws/gcloud/az, backs `login`/`status`/`logout` and the apply/delete preflight
internal/fleetinfra/          SDK converge engine behind `fleet bootstrap`
internal/provisioner/{aws,gcp,azure}   ClusterProvisioner + IdentityProvisioner + NetworkProvisioner impls (EKS/GKE/AKS)
internal/repo/                RepoProvisioner over GitHub Enterprise (go-github): Exists/Create/Clone/Push/Archive
internal/registry/            Fleet Registry client + lease/locking
internal/catalog/             profile resolution (tier-small/standard/regulated + per-cluster override patches)
internal/argocd/              app-of-apps manifest rendering, ingress/Gateway access-mode templating, Argo CD install
internal/orchestrator/        per-cluster phase state machine (apply) and reverse teardown (delete)
internal/fleet/               fleet-wide operations: audit, update, status, all read/write through internal/registry
internal/ingestion/           Central Ingestion API's verification (JWT/JWKS, per-cluster issuer binding) and write path
internal/reporter/            fleet-status-reporter's Argo CD summary + signed push logic
internal/tools/docsgen/       regenerates docs/cli/*.md from the cobra command tree (`make docs`)
```

`internal/identity` in the original plan ended up folded into
`internal/provisioner/{aws,gcp,azure}` instead of its own package —
`IdentityProvisioner` is cloud-specific enough that keeping it beside its
cloud's `ClusterProvisioner` reads better than a same-named type spread
across two directories per cloud.

Shared domain types (`ClusterID`, `ClusterSpec`, `Profile`, `AddonRef`) live in `internal/core` and are consumed by all of the above.

## Cluster repo contract

Each provisioned cluster's GitHub repo holds three files, and their roles must stay distinct:

- `cluster.yaml` — desired infra (provider, region, access mode, node pools). Drives cloud SDK calls; `fleet audit` diffs live infra against it.
- `addons.yaml` — resolved addon set (profile + override patch, flattened without duplicating the base). Argo CD syncs from this.
- `.state.yaml` — last-applied hash used for idempotent diffing. Not user-authored.

Addons are delivered app-of-apps: one root Argo CD Application discovers one Application per addon, so addons sync and fail independently. Argo CD itself is installed via the Helm Go library (`helm.sh/helm/v3/pkg/action`), never by shelling out to `helm` or `kubectl`. The cluster's own repository is always created private, so `apply` also applies a `repo-creds` Secret (`argocd.argoproj.io/secret-type: repository`) alongside the root Application — without it Argo CD's first reconcile fails with "authentication required" and never discovers a single addon.

## CLI usage

Cloud auth is CLI-session-based, not env vars — `kubespin` reuses whatever `aws sso login` / `gcloud auth application-default login` / `az login` already cached. `kubespin login` drives all three concurrently; `kubespin status` reports session validity without changing anything; every apply/delete preflights auth itself and fails fast with "run kubespin login" rather than a cryptic SDK error mid-provision. Full reference: [docs/cli/](docs/cli/).

```bash
# Auth
kubespin login                                    # log in to every configured provider
kubespin login --only aws,gcp                     # just these
kubespin status                                   # session validity per provider, never fails the command
kubespin logout --only azure

# Fleet infra (once per fleet account, before any cluster)
make lambda
kubespin fleet bootstrap --account-id <12-digit-id> --registry-region us-east-1 --dry-run
kubespin fleet bootstrap --account-id <12-digit-id> --registry-region us-east-1

# Apply — AWS, letting kubespin create the VPC/subnets
kubespin apply --provider aws --cluster-id eks-demo-01 --region us-east-1 \
  --access private --github-org GitOpsHub --profile tier-small@1.0.0 \
  --registry-region us-east-1 --dry-run

# Apply — GCP, same idea (subnetwork auto-created if --subnets is omitted)
kubespin apply --provider gcp --gcp-project my-project --cluster-id gke-demo-01 \
  --region us-central1 --access private --github-org GitOpsHub \
  --profile tier-small@1.0.0 --registry-region us-east-1

# Apply — Azure, with an operator-supplied subnet instead of an auto-created one
kubespin apply --provider azure --azure-subscription <subscription-id> \
  --cluster-id aks-demo-01 --region eastus --access private \
  --subnets /subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Network/virtualNetworks/<vnet>/subnets/<subnet> \
  --github-org GitOpsHub --profile tier-small@1.0.0 --registry-region us-east-1

# Fleet-wide (operate across clusters, filtered rather than single-cluster)
kubespin fleet status --stale-only
kubespin fleet audit --provider aws --github-org GitOpsHub
kubespin fleet update --component argo-rollouts --version 1.7.0 --github-org GitOpsHub

# Teardown — reverse of apply: decommission mark → IAM/OIDC cleanup → cluster delete → repo archive
kubespin delete --cluster-id eks-demo-01 --registry-region us-east-1 --dry-run
kubespin delete --cluster-id eks-demo-01 --registry-region us-east-1
```

`--registry-region` has no default — it must come from the flag, `KUBESPIN_REGISTRY_REGION`, or the config file, since silently defaulting risks splitting a fleet across two registries with no error. See [.env.example](.env.example) for the env vars real (non-dry-run) `apply`/`delete` need (`GITHUB_TOKEN`, `GITHUB_ORG`, `KUBESPIN_REGISTRY_REGION`).

## Working with the plan

Milestones are gates — the plan explicitly says not to start the next until the current one's acceptance criteria pass **on all three clouds**. M2 (cluster + identity provisioning) is the hard gate before any addon work. When completing work, check off the relevant boxes in the plan rather than tracking status elsewhere.
