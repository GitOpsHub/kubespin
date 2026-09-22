# On-call runbook

This is what to do when a cluster operation goes wrong. For *why* the system
is shaped the way it is, see [Architecture](architecture.md); for exact flags,
see the [CLI reference](cli/kubespin.md).

kubespin runs only when an operator runs it: there is no service to page for
and nothing reporting in the background. So everything below is about a
command that failed part-way through — an `apply` that stopped at a phase, or
a `delete` that cannot finish.

## `apply` is stuck or keeps failing

`apply` is idempotent and resumable: a retried `apply` re-enters at
whatever phase the cluster registry last recorded
([`Orchestrator.Apply`](https://github.com/GitOpsHub/kubespin/blob/main/internal/orchestrator/orchestrator.go)), so the
first response to a failed `apply` is almost always **run it again**. It is
not a special "retry" mode — it is the same command.

If a retry doesn't help:

1. **Read the phase it's stuck at.** The error names the failing step
   (`"%s: %w"`, step name first), and the command prints
   `cluster <id> stopped at phase <phase>` before it. A dry run reports the
   same phase without touching anything:

   ```bash
   kubespin apply --spec ./cluster.yaml --dry-run
   ```

2. **Check whether another run holds the lease.** `ErrBusy` means someone
   else's `apply` (or a crashed one whose lease hasn't expired yet) is
   holding it. Leases expire on their own
   (`DefaultLeaseTTL`, 15 minutes) — wait it out rather than trying to force
   past it. There is deliberately no "break the lease" command: a forced
   takeover while the original run is still alive is exactly the double-apply
   race the lease exists to prevent.

3. **Cloud-side failures** (quota, a transient API error, a permissions
   gap) surface with the cloud SDK's own error wrapped in the step name.
   Fix the underlying cause — a quota increase, an IAM policy — and retry;
   `apply` does not need to be told what changed, because `Create` and
   `Reconcile` are idempotent, and `ensureNodeGroups` never deletes, so a
   partial node pool from a failed run is picked up and completed rather
   than duplicated.

4. **Repo-side failures** (GitHub rate limits, a missing `GITHUB_TOKEN`,
   branch protection conflicts) fail at `PhaseClusterCreated` (initial seed)
   or during the ready-cluster reconcile
   ([`ReadyReconcile`](https://github.com/GitOpsHub/kubespin/blob/main/internal/orchestrator/steps.go)).

5. **If a cluster has been stuck at the same phase across several retries
   with the same error**, stop retrying blind and read the actual cloud
   state before the next attempt — a retry loop
   against a genuinely broken precondition (a deleted subnet, a revoked
   credential) just wastes lease cycles. Read the cloud's own console or CLI,
   and the cluster's repository for `.state.yaml` and `addons.yaml`.

## Troubleshooting a stuck delete

`delete` is idempotent and resumable exactly like `apply`: a retried `delete`
picks up from `decommissioning` and every step it re-runs (load balancer
drain, cluster delete, network teardown) converges rather than erroring on
something already gone. So the first response to a failed `delete` is the
same as for `apply` — **run it again**:

```bash
kubespin delete --provider aws --cluster-id my-cluster --region us-east-1 \
  --access public --github-org "$GITHUB_ORG" --yes
```

If it fails the same way twice, the blocker is usually a resource kubespin
didn't create itself — its deterministic teardown only knows how to remove
what its own deterministic-naming `Create`/`EnsureNetwork` provisioned, not
anything added by hand afterward. Two real examples:

1. **IAM `DeleteConflict: Cannot delete entity, must delete policies first`
   on a node role.** `deleteRole` only detaches *attached* (managed)
   policies before deleting a role — it does not know about inline
   policies, since kubespin itself never attaches one. Someone adding
   `aws iam put-role-policy` to a node role by hand (e.g. to grant an
   addon extra permissions ad hoc) leaves an inline policy that blocks
   `DeleteRole`. Find and remove it, then retry:

   ```bash
   aws iam list-role-policies --role-name kubespin-my-cluster-node
   aws iam delete-role-policy --role-name kubespin-my-cluster-node \
     --policy-name <name-from-above>
   ```

2. **EC2 `DependencyViolation: The vpc '...' has dependencies and cannot be
   deleted`, after node groups and the cluster itself are already gone.**
   `EnsureNetwork`/`DeleteNetwork` only track what they themselves created —
   a VPC/subnet, an Internet Gateway, a route table. A manually-created VPC
   Peering Connection (e.g. to reach an EFS filesystem in another VPC) is
   invisible to that teardown and blocks `DeleteVpc`. List what's actually
   attached to the VPC and remove anything kubespin didn't create:

   ```bash
   aws ec2 describe-vpc-peering-connections \
     --filters "Name=requester-vpc-info.vpc-id,Values=<vpc-id>"
   aws ec2 delete-vpc-peering-connection \
     --vpc-peering-connection-id <pcx-id-from-above>
   ```

   A leftover ENI (still detaching from a just-deleted node) produces the
   same error but clears on its own within a minute or two — check
   `aws ec2 describe-network-interfaces --filters Name=vpc-id,Values=<vpc-id>`
   returns empty before assuming it's a manually-added dependency rather
   than ordinary AWS eventual consistency.

In both cases: the fix is always "identify the resource kubespin's
deterministic teardown doesn't know about, remove it by hand, then retry
`kubespin delete`" — never a reason to force-delete the cluster's registry
record out from under a resource that's still real.
