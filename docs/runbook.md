# On-call runbook

This is what to do when something in the fleet is broken. For *why* the
system is shaped the way it is, see [Architecture](architecture.md); for
exact flags, see the [CLI reference](cli/kubespin.md).

Three things can go wrong, and they map directly onto the three services in
[the diagram](architecture.md): a cluster stops reporting, an `apply` gets
stuck, or the Central Ingestion API itself is down.

## A cluster is stale

```bash
kubespin fleet status --stale-only
```

That lists every cluster that has missed its reporting window
([`Record.Stale`](https://github.com/GitOpsHub/kubespin/blob/main/internal/registry/registry.go), threshold set by
`--stale-threshold`, default 10 minutes — long enough to tolerate a couple of
missed 2-3 minute pushes without paging on noise).

Staleness is a statement about *missing reports*, not about reachability:
nothing in `fleet status` ever connects to a cluster, so a stale cluster and
an unreachable one look the same from here. Work outward from that:

1. **Check the cluster's own fleet-status-reporter CronJob**, if you have
   any access path to it (this is the one place a human occasionally does
   need cluster access, outside kubespin's own outbound-only design — treat
   it as a break-glass action, not routine). Look for:
   - The CronJob not scheduling at all (suspended, or the cluster's control
     plane itself is down).
   - Pods failing before they can push — check `ARGOCD_SERVER`,
     `ARGOCD_TOKEN`, `INGESTION_URL`, and `IDENTITY_TOKEN_PATH` are all set
     and the token file exists (see [cmd/fleet-status-reporter](https://github.com/GitOpsHub/kubespin/tree/main/cmd/fleet-status-reporter)).
   - Pods pushing but getting rejected — see "the ingestion API is
     rejecting pushes" below.

2. **Check whether the cluster's OIDC issuer is even recorded.** `fleet
   status` reads the Fleet Registry directly; if `apply` never completed
   identity binding (`PhaseClusterCreated` -> `PhaseIdentityBound`), no
   issuer was ever recorded ([`RecordOIDCIssuer`](https://github.com/GitOpsHub/kubespin/blob/main/internal/registry/registry.go)),
   and every push from that cluster will be rejected as `invalid_token` —
   indistinguishable from staleness until you check the ingestion API's
   logs (below).

3. **If the cluster is confirmed down or decommissioned outside kubespin**,
   don't just ignore the staleness — either restore it or run
   `kubespin delete` so the registry reflects reality. A stale entry
   that stays stale forever erodes trust in the whole `fleet status` view.

## `apply` is stuck or keeps failing

`apply` is idempotent and resumable: a retried `apply` re-enters at
whatever phase the Fleet Registry last recorded
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
   branch protection conflicts) fail at `PhaseIdentityBound` (initial seed)
   or during the ready-cluster reconcile
   ([`ReadyReconcile`](https://github.com/GitOpsHub/kubespin/blob/main/internal/orchestrator/steps.go)). GitHub API rate
   limits are the sharp edge here at fleet scale — see "fleet-wide
   operations are rate-limited" below before assuming it's a one-off.

5. **If a cluster has been stuck at the same phase across several retries
   with the same error**, stop retrying blind and read the actual cloud
   state before the next attempt — a retry loop
   against a genuinely broken precondition (a deleted subnet, a revoked
   credential) just wastes lease cycles.

   ```bash
   kubespin fleet audit --provider aws \
     --github-org "$GITHUB_ORG"
   ```

   Or read the cluster's own repository for `.state.yaml` and `addons.yaml`.

## The Central Ingestion API is down or rejecting pushes

The ingestion API ([cmd/ingestion](https://github.com/GitOpsHub/kubespin/tree/main/cmd/ingestion),
[internal/ingestion](https://github.com/GitOpsHub/kubespin/tree/main/internal/ingestion)) is the *only* inbound surface
in the whole system. If it's down, every cluster's status reporting is
blind simultaneously — that will look like a mass staleness event in `fleet
status`, not scattered individual cluster problems, which is the fastest way
to tell "the API is down" apart from "several clusters independently broke".

**It is a stateless Lambda behind API Gateway; it holds no state of its
own** — every write goes straight to the Fleet Registry. Restoring it (a
redeploy, a Lambda concurrency limit increase, an API Gateway throttle
adjustment) does not require any cluster-side action; every
fleet-status-reporter CronJob keeps retrying on its own schedule and will
converge automatically once the API is back. There's no backlog to drain.

If clusters are reporting but getting rejected (a spike in 4xx rather than a
full outage), the response body names why
([`Response.Error`](https://github.com/GitOpsHub/kubespin/blob/main/internal/ingestion/handler.go)):

| `error` | Meaning | Action |
|---|---|---|
| `missing_token` | No `Authorization: Bearer` header | Check the reporting cluster's projected token volume is mounted |
| `unknown_cluster` | `{clusterId}` isn't in the Fleet Registry | The cluster was never registered, or was decommissioned — confirm before assuming it's an attack |
| `invalid_token` | Signature, expiry, or issuer mismatch | See below — this is the anti-replay check firing |
| `wrong_subject` | Token isn't from `fleet-status-reporter`'s service account | Some other in-cluster workload has a token from the same issuer and tried to push — investigate, this should not happen from kubespin's own components |
| `wrong_audience` | Token wasn't scoped to the ingestion API | Check the projected token volume's `audience` field matches `ExpectedAudience` |
| `invalid_body` | Status payload isn't valid JSON | A fleet-status-reporter version mismatch, most likely |

A burst of `invalid_token` for one specific cluster, right after that
cluster's `apply` ran, usually means identity binding produced a new OIDC
issuer (a cluster recreation, not just an update) and the Fleet Registry's
recorded issuer is stale — `RecordOIDCIssuer` only ever writes once per
`bindIdentityStep`, so this can only happen if the cluster's identity was
rebuilt outside a normal `apply` resume.

## fleet-wide operations are rate-limited

`fleet update` and `fleet audit` fan out across the whole registry with a
bounded worker pool (`--concurrency`, default 4). GitHub Enterprise's API
rate limit is the constraint to watch at scale — raise `--concurrency`
cautiously and watch for a spike in per-cluster failures in the command's
own output (`N cluster(s), M failed`) rather than assuming higher
concurrency is free.

Both need `--github-org` and `GITHUB_TOKEN`, since both read cluster
repositories; `fleet audit` additionally needs `--gcp-project` /
`--azure-subscription` if the fleet holds clusters on those clouds, or those
clusters report `FAILED` rather than being skipped.

Both commands report every cluster's outcome independently
([`AuditResult`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/fleet.go),
[`UpdateResult`](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleet/update.go)) — one cluster's failure never
aborts the run. A failed wave is safe to simply re-run: `fleet update` is
idempotent (a cluster already at the target version reports "already up to
date" and commits nothing), and `fleet audit` is read-only.
