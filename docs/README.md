# kubespin documentation

kubespin provisions Kubernetes clusters across EKS, GKE, and AKS. Each cluster
gets its own Git repository and its own local Argo CD that syncs from it — there
is no central Argo CD hub, and nothing ever reaches inbound into a cluster.

## Contents

| Document | Read it when |
|---|---|
| [Architecture](architecture.md) | You need to understand *why* the system is shaped this way before changing it |
| [Examples](examples.md) | You want a working command to copy-paste for a specific scenario |
| [Fleet bootstrap](fleet-bootstrap.md) | You are provisioning the shared fleet infrastructure, or something went wrong doing so |
| [Runbook](runbook.md) | Something in the fleet is broken and you're on call |
| [Development](development.md) | You are writing code in this repository |
| [CLI reference](cli/kubespin.md) | You want the exact flags for a command |
| [Code organization](code-organization.md) | You want to know which package owns something, or where new code belongs |
| [Code reference](reference/index.md) | You want the exported types and methods of a specific `internal/*` package |

The CLI reference is generated from the command tree by `make docs`. Do not edit
it by hand — CI regenerates it and fails on any difference.

Commands throughout these docs are written as plain `kubespin`, which is what
`make build` installs onto your `PATH` (see [Development](development.md)).
Every example carries the flags that command actually requires, and a test
parses each one against the real command tree — they are meant to run as
written. The `kubespin <command> [flags]` line at the top of each reference
page is cobra's usage synopsis, not a runnable command.

## Where the project is

**Every command is implemented: `apply`, `delete`, and every `fleet`
subcommand (`bootstrap`, `update`, `audit`, `status`).** Nothing left in the
CLI is a stub. What that means, and does not mean, varies by milestone —
see [IMPLEMENTATION-PLAN-multicloud-k8s-platform-cli.md](https://github.com/GitOpsHub/kubespin/blob/main/IMPLEMENTATION-PLAN-multicloud-k8s-platform-cli.md)
for the acceptance criteria each milestone is actually gated on, milestone by
milestone. In short:

- **M0-M4, M6, M8, M9 are done** at the package level, fully unit-tested
  against fakes (cloud SDK fakes, an in-memory registry, an in-memory
  GitHub-shaped repo). None of it has run against a real cloud account, a
  real GitHub org, or a real Kubernetes cluster — that verification needs
  live infrastructure this environment does not have, and is called out
  explicitly wherever it applies.
- **M5 (Argo CD bootstrap) is partial.** App-of-apps manifest rendering and
  ingress access-mode templating are done and wired in
  ([internal/argocd](https://github.com/GitOpsHub/kubespin/tree/main/internal/argocd)). The actual Helm-as-library
  install is not: an early attempt pulled in Helm's full transitive
  dependency tree (~450 lines of new `go.sum` — OCI registry, Prometheus,
  Redis, SQL drivers) for code with no caller yet, since minting a
  `*rest.Config` for a freshly created cluster needs a per-cloud token
  scheme (IAM-signed / Google OAuth / Azure AD) that doesn't exist. That
  trade wasn't worth it, so it was reverted rather than merged half-working.
- **M7 (`tier-standard`/`tier-regulated`) is data-complete, verification-
  incomplete.** Both profiles resolve and validate; whether Kyverno actually
  denies what it's supposed to, whether Velero actually restores a PVC —
  those need a live cluster with those addons running, same as M5.
- **M10's load test and runbook are done**
  ([internal/fleet/loadtest_test.go](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/loadtest_test.go),
  [runbook.md](runbook.md)); pilot team onboarding is an organizational
  rollout step, not something to build.

See [internal/core](https://github.com/GitOpsHub/kubespin/tree/main/internal/core) for the shared domain types,
[internal/registry](https://github.com/GitOpsHub/kubespin/tree/main/internal/registry) for the Fleet Registry client and
lease, [internal/orchestrator](https://github.com/GitOpsHub/kubespin/tree/main/internal/orchestrator) for the per-cluster
phase state machine `apply` walks and the reverse teardown `delete` walks,
and [internal/fleet](https://github.com/GitOpsHub/kubespin/tree/main/internal/fleet) for the fleet-wide operations
(`audit`/`update`/`status`) that fan out across it.

## Open questions

Three decisions are still outstanding, and each blocks a specific milestone
from moving past "implemented against fakes" to "verified for real":

1. **GitHub Enterprise Server or Enterprise Cloud?** Changes how the
   `go-github` client is constructed and the rate-limit budget fleet-wide
   operations are designed against. Blocks live verification of M3, M4, M8, M9.
2. **The fleet AWS account ID and region.** Needed to actually run
   `fleet bootstrap`; everything up to that point is verified without them.
3. **Per-cloud cluster credential acquisition** (an IAM-signed token for
   EKS, a Google OAuth token for GKE, an Azure AD token for AKS) — how
   `apply` gets a `*rest.Config` for a cluster it just created. This is
   what's actually blocking M5's Helm-as-library Argo CD install; it's a
   real design question, not an implementation detail, and deserves its own
   decision rather than being guessed at inside a milestone that assumes
   it's already solved.
