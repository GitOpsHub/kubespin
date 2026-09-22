---
slug: /
title: kubespin documentation
---

# kubespin documentation

kubespin provisions Kubernetes clusters across EKS, GKE, and AKS. Each cluster
gets its own Git repository and its own local Argo CD that syncs from it — there
is no central Argo CD hub, and nothing ever reaches inbound into a cluster.

## Contents

| Document | Read it when |
|---|---|
| [Quickstart](examples.md#quickstart) | You just cloned this and want one cluster running, fastest path |
| [Architecture](architecture.md) | You need to understand *why* the system is shaped this way before changing it |
| **Getting started** | |
| [Examples](examples.md) | You want a working command to copy-paste for a specific scenario |
| [Low-cost dev clusters](low-cost-dev-clusters.md) | You're on a cloud free tier and want the cheapest cluster for learning Kubernetes |
| [Autopilot clusters](autopilot-clusters.md) | You want fully-managed compute (GKE Autopilot or EKS Auto Mode) |
| **Operations** | |
| [Runbook](runbook.md) | An `apply` or `delete` is stuck and you're on call |
| [CI/CD with Azure OIDC](cicd-azure-oidc.md) | You're wiring up a GitHub Actions workflow to run kubespin against AWS/Azure |
| **Reference** | |
| [CLI reference](cli/kubespin.md) | You want the exact flags for a command |
| [Code organization](code-organization.md) | You want to know which package owns something, or where new code belongs |
| [Code reference](reference/index.md) | You want the exported types and methods of a specific `internal/*` package |
| [Development](development.md) | You are writing code in this repository |

The CLI reference is generated from the command tree by `make docs`. Do not edit
it by hand — CI regenerates it and fails on any difference.

Commands throughout these docs are written as plain `kubespin`, which is what
`make build` installs onto your `PATH` (see [Development](development.md)).
Every example carries the flags that command actually requires, and a test
parses each one against the real command tree — they are meant to run as
written. The `kubespin <command> [flags]` line at the top of each reference
page is cobra's usage synopsis, not a runnable command.

## Project status

**Every command is implemented: `apply`, `delete`, `login`, `status`, and
`logout`.** Nothing in the CLI is a stub.

- **Unit and fake-based tests**: Core domain models, catalog resolution,
  orchestrator phase state machines, repo provisioners, and cloud provisioners
  are thoroughly unit-tested against fakes (an in-memory registry, an in-memory
  GitHub-shaped repo, and cloud SDK fakes).
- **Argo CD bootstrap**: App-of-apps manifest rendering and ingress access-mode
  templating ([internal/argocd](reference/argocd.md)) are fully wired. The
  Helm install executes via `argocd.HelmInstaller` (`helm upgrade --install`
  semantics through `helm.sh/helm/v3/pkg/action`) against a `*rest.Config`
  minted per cloud (`provisioner.RESTConfigProvisioner` across AWS STS, GCP ADC,
  and Azure credentials). The root Application is applied directly via
  `argocd.KubeApplier` using client-go dynamic client with server-side apply.
- **Addon sizing**: Builtin sizes (`small`, `medium`, `large`) resolve and
  validate through [internal/catalog](reference/catalog.md), supporting
  per-cluster overrides.
- **Runbooks**: Operations documentation ([runbook.md](runbook.md)) is in
  place.

See [internal/core](reference/core.md) for the shared domain types,
[internal/registry](reference/registry.md) for the cluster registry client and
lease, and [internal/orchestrator](reference/orchestrator.md) for the
per-cluster phase state machine `apply` walks and the reverse teardown
`delete` walks.
