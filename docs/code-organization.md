# Code organization

This page maps the repository's packages to what they're responsible for and
how they depend on each other. For *why* the system is shaped this way, see
[Architecture](architecture.md). For method-level detail on any package below,
see the [code reference](reference/index.md).

## Directory layout

```text
cmd/kubespin/                 main() — delegates entirely to internal/cli.NewRootCommand
cmd/ingestion/                Central Ingestion API handler (Go, deployed to Lambda)
cmd/fleet-status-reporter/    in-cluster CronJob: queries local Argo CD, pushes signed status

internal/cli/                 cobra command tree: apply, delete, fleet bootstrap|update|audit|status, login, status, logout
internal/core/                shared domain types: ClusterID, ClusterSpec, ClusterSize, Profile, AddonRef, Access, NodePool
internal/auth/                operator-facing cloud auth: shells out to aws/gcloud/az
internal/fleetinfra/          SDK converge engine behind `fleet bootstrap`
internal/provisioner/{aws,gcp,azure}   ClusterProvisioner + IdentityProvisioner + NetworkProvisioner impls (EKS/GKE/AKS)
internal/repo/                RepoProvisioner over GitHub Enterprise (go-github): Exists/Create/Clone/Push/Archive
internal/registry/            Fleet Registry client + lease/locking
internal/catalog/             size resolution (small/medium/large, fully builtin) + per-cluster override patches
internal/argocd/              app-of-apps manifest rendering, ingress/Gateway access-mode templating, Argo CD install
internal/orchestrator/        per-cluster phase state machine (apply) and reverse teardown (delete)
internal/fleet/               fleet-wide operations: audit, update, status, dashboard
internal/ingestion/           Central Ingestion API's verification (JWT/JWKS, per-cluster issuer binding) and write path
internal/reporter/            fleet-status-reporter's Argo CD summary + signed push logic
internal/tools/docsgen/       regenerates docs/cli/*.md from the cobra command tree (`make docs`)
internal/version/             build-time version metadata
```

`internal/identity` in the original design doc ended up folded into
`internal/provisioner/{aws,gcp,azure}` instead of its own package —
`IdentityProvisioner` is cloud-specific enough that keeping it beside its
cloud's `ClusterProvisioner` reads better than a same-named type spread
across two directories per cloud.

## How a package finds another package

`internal/core` sits at the bottom: it defines `ClusterID`, `ClusterSpec`,
`ClusterSize`, `Profile`, `AddonRef`, and the phase enum, and nothing in
`internal/core` imports anything else in the repo. Every other package
imports it.

Above that, packages split into two groups that don't import each other
directly — they're connected only through `internal/orchestrator`, which is
where a command's actual work happens:

- **Provisioning side** — `internal/provisioner/{aws,gcp,azure}` implement the
  `ClusterProvisioner`/`IdentityProvisioner`/`NetworkProvisioner` interfaces
  declared in `internal/provisioner`. `internal/auth` sits beside these,
  giving `internal/cli` the operator's cloud session before any provisioner
  call is attempted.
- **GitOps side** — `internal/repo` owns the cluster's GitHub repository;
  `internal/catalog` resolves which addons belong in it; `internal/argocd`
  renders the app-of-apps manifests that go into it and performs the
  Helm-SDK install of Argo CD itself.

`internal/orchestrator` drives both sides through one `apply`: it asks a
`ClusterProvisioner` to reconcile infrastructure, an `IdentityProvisioner` to
bind workload identity, then `internal/repo` + `internal/catalog` +
`internal/argocd` to push the addon set — writing a phase transition to
`internal/registry` after each step. `internal/fleet` is the read side of the
same registry: `audit`/`status`/`update` never touch a provisioner directly
except when `update` needs to push a new addon version through the same
GitOps path `orchestrator` uses.

`internal/cli` is the only package that imports command-level packages
(`auth`, `orchestrator`, `fleet`, `fleetinfra`) together — it exists purely to
wire cobra flags to their calls, which is why
[internal/cli's reference doc](reference/cli.md) reads as "which package does
this command call" rather than new logic of its own.

`internal/ingestion` and `internal/reporter` are the one place the outbound-only
invariant shows up in code: `internal/reporter` runs *inside* a cluster and
only ever makes one outbound POST; `internal/ingestion` runs in the Central
Ingestion API's Lambda and only ever receives that POST, verifies it, and
writes to `internal/registry`. Neither imports any provisioner or repo
package — they have no way to reach into a cluster even if something asked
them to.

```mermaid
flowchart TB
    core["internal/core<br/>(domain types, imported by everything)"]

    subgraph provisioning["Provisioning side"]
        auth["internal/auth"]
        provisioner["internal/provisioner<br/>+ aws / gcp / azure"]
    end

    subgraph gitops["GitOps side"]
        repo["internal/repo"]
        catalog["internal/catalog"]
        argocd["internal/argocd"]
    end

    orchestrator["internal/orchestrator"]
    registry["internal/registry"]
    fleet["internal/fleet"]
    cli["internal/cli"]
    fleetinfra["internal/fleetinfra"]

    reporter["internal/reporter<br/>(runs in-cluster)"]
    ingestion["internal/ingestion<br/>(runs in Lambda)"]

    cli --> auth
    cli --> orchestrator
    cli --> fleet
    cli --> fleetinfra

    orchestrator --> provisioning
    orchestrator --> gitops
    orchestrator --> registry
    fleet --> registry
    fleet -.->|update| gitops

    provisioning --> core
    gitops --> core

    reporter -->|"signed POST, outbound only"| ingestion
    ingestion --> registry
```

## Where to add new code

- **A new cloud provider** implements `ClusterProvisioner`,
  `IdentityProvisioner`, and `NetworkProvisioner` in a new
  `internal/provisioner/<cloud>` package — `internal/orchestrator` and
  `internal/cli` need no changes, per the architecture invariant that no
  cloud conditionals leak outside `internal/provisioner/*`.
- **A new fleet-wide command** (alongside `audit`/`update`/`status`) belongs
  in `internal/fleet`, reading/writing only through `internal/registry`, then
  gets a thin cobra wrapper in `internal/cli`.
- **A new addon profile or tier** belongs in `internal/catalog`.
- **A new phase or transition rule** belongs in `internal/core`'s phase state
  machine — `internal/registry` validates against it on every write, so
  changing it there is enough to enforce it everywhere.
