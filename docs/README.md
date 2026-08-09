# kubespin documentation

kubespin provisions Kubernetes clusters across EKS, GKE, and AKS. Each cluster
gets its own Git repository and its own local Argo CD that syncs from it — there
is no central Argo CD hub, and nothing ever reaches inbound into a cluster.

## Contents

| Document | Read it when |
|---|---|
| [Architecture](architecture.md) | You need to understand *why* the system is shaped this way before changing it |
| [Fleet bootstrap](fleet-bootstrap.md) | You are provisioning the shared fleet infrastructure, or something went wrong doing so |
| [Development](development.md) | You are writing code in this repository |
| [CLI reference](cli/kubespin.md) | You want the exact flags for a command |

The CLI reference is generated from the command tree by `make docs`. Do not edit
it by hand — CI regenerates it and fails on any difference.

## Where the project is

**Milestones M0 (foundations) and M1 (registry) are complete.** What exists and
works:

- `kubespin fleet bootstrap` — provisions the Fleet Registry and Central
  Ingestion API through the AWS SDK
- Shared domain types and the cluster phase state machine
  ([internal/core](../internal/core))
- The Fleet Registry client and the lease that serialises concurrent `apply`
  runs ([internal/registry](../internal/registry))
- The orchestrator that walks a cluster through the phases and resumes a failed
  run from where it stopped ([internal/orchestrator](../internal/orchestrator))
- The command tree with layered configuration
  ([internal/cli](../internal/cli))
- The ingestion Lambda handler, as a deliberate 501 skeleton
  ([cmd/ingestion](../cmd/ingestion))

**The orchestrator's steps are no-ops.** The state machine, lease, resumption,
and registry writes are all real and tested; the work each phase performs is not
built yet. Cluster and identity provisioning arrive in M2, repository seeding in
M3, and the Argo CD bootstrap in M5.

**The commands are still stubs.** `apply`, `delete`, `fleet update`,
`fleet audit`, and `fleet status` parse their flags and then exit 3 with "not
implemented yet". They fail loudly on purpose: a stub that exited 0 would imply
it had done something.

M2 is next — the `ClusterProvisioner` and `IdentityProvisioner` interfaces and
their three cloud implementations. See
[EXECUTION-PLAN.md](../EXECUTION-PLAN.md) for the milestone breakdown and
[IMPLEMENTATION-PLAN-multicloud-k8s-platform-cli.md](../IMPLEMENTATION-PLAN-multicloud-k8s-platform-cli.md)
for the acceptance criteria each milestone is gated on.

## Open questions

Two decisions are still outstanding and block parts of M1 and M3:

1. **GitHub Enterprise Server or Enterprise Cloud?** Changes how the `go-github`
   client is constructed and the rate-limit budget that fleet-wide operations
   are designed against.
2. **The fleet AWS account ID and region.** Needed to actually run
   `fleet bootstrap`; everything up to that point is verified without them.
