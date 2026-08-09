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

- The provisioner interfaces and their **AWS** implementation — EKS clusters,
  IRSA workload identity, and the status reporter's egress path
  ([internal/provisioner](../internal/provisioner))
- `kubespin apply`, wired end to end through the registry, the orchestrator, and
  the AWS provisioner

**M2 is half done, deliberately.** AWS is implemented first so the interfaces
are proven against a real cloud before GCP and Azure are built against them —
the alternative is two teams building on a shape that shifts underneath them.
`apply --provider gcp` and `--provider azure` fail with a clear statement rather
than a generic error.

**Two phases still do nothing.** A run reaches ready with a real cluster and a
real workload identity, but no repository and no addons: repository seeding
arrives in M3 and the Argo CD bootstrap in M5.

**`delete` and the `fleet` subcommands other than `bootstrap` are still stubs**,
exiting 3 with "not implemented yet". They fail loudly on purpose: a stub that
exited 0 would imply it had done something.

Next is the rest of M2 — GKE and AKS against the now-proven interfaces. See
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
