# Code reference

Package-by-package reference for every `internal/*` package and the thin
`cmd/*` entrypoints: exported types, their fields and methods, exported
functions, and the invariants the code actually enforces. For how these
packages fit together as a system, see [Architecture](../architecture.md);
for the package layout at a glance, see [Code organization](../code-organization.md).
For CLI flags, see the [CLI reference](../cli/kubespin.md) instead — these
pages describe code structure, not flags.

## Domain and shared interfaces

| Package | Covers |
|---|---|
| [internal/core](core.md) | `ClusterID`, `ClusterSpec`, `Profile`, `AddonRef`, `Access`, `NodePool`, the phase state machine |

## Provisioning

Start with the [AWS vs. GCP vs. Azure comparison](provisioners.md) if you want
the three clouds side by side; the per-cloud pages below are the full
method-level reference.

| Package | Covers |
|---|---|
| [internal/provisioner (shared) + internal/provisioner/aws](provisioner-aws.md) | `ClusterProvisioner`/`IdentityProvisioner`/`NetworkProvisioner` interfaces; EKS, IRSA, VPC auto-creation |
| [internal/provisioner/gcp](provisioner-gcp.md) | GKE, Workload Identity, VPC/subnetwork, Cloud Router + NAT |
| [internal/provisioner/azure](provisioner-azure.md) | AKS, federated credential + managed identity, resource group/VNet/subnet |

## Fleet infrastructure and state

| Package | Covers |
|---|---|
| [internal/fleetinfra](fleetinfra.md) | SDK converge engine behind `fleet bootstrap` (registry table, ingestion Lambda, API Gateway) |
| [internal/registry](registry.md) | Fleet Registry client, DynamoDB/in-memory implementations, conditional-write lease |
| [internal/fleet](fleet.md) | Fleet-wide `audit`/`update`/`status`/`dashboard` operations |

## Cluster repo and addons

| Package | Covers |
|---|---|
| [internal/repo](repo.md) | GitHub-backed `RepoProvisioner`: Exists/Create/Clone/Push/Archive, `cluster.yaml`/`addons.yaml`/`.state.yaml` |
| [internal/catalog](catalog.md) | Profile resolution: tier-small/standard/regulated + per-cluster override patches |
| [internal/argocd](argocd.md) | App-of-apps rendering, ingress/Gateway access-mode templating, Helm-SDK Argo CD install |
| [internal/orchestrator](orchestrator.md) | Per-cluster phase state machine driving `apply` (split-diff) and `delete` (reverse teardown) |

## Auth and CLI

| Package | Covers |
|---|---|
| [internal/auth](auth.md) | Operator-facing cloud auth (`login`/`status`/`logout`, apply/delete preflight) |
| [internal/cli](cli.md) | The cobra command tree wiring every package above to a command |

## Central Ingestion API and in-cluster reporter

| Package | Covers |
|---|---|
| [internal/ingestion](ingestion.md) | JWT/JWKS verification, per-cluster issuer binding, registry write path |
| [internal/reporter](reporter.md) | fleet-status-reporter's Argo CD summary + signed push logic |

## Entrypoints and tooling

| Package | Covers |
|---|---|
| [Entrypoints and tooling](entrypoints.md) | `cmd/kubespin`, `cmd/ingestion`, `cmd/fleet-status-reporter`, `internal/tools/docsgen`, `internal/version` |
