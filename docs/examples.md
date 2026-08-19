# Examples

Working commands to copy, paste, and adjust. For flag-by-flag detail see the
[CLI reference](cli/kubespin.md); for *why* the system behaves this way see
[Architecture](architecture.md); for the fleet-bootstrap deep dive
(permissions, what it creates, troubleshooting) see
[Fleet bootstrap](fleet-bootstrap.md).

Every command below is written as `kubespin`, run from the root of a
repository checkout after `make build`. Nothing here is a sketch: each
example carries every flag the command actually requires, so it runs as
written once the prerequisites below are in place.

```bash
make build
```

## Prerequisites

### Cloud sessions

kubespin authenticates to clouds through your existing CLI sessions, not
environment variables — run the provider's own login first, or let
`kubespin login` do it for you (see [Auth workflows](#auth-workflows)):

```bash
aws sso login
```

```bash
gcloud auth application-default login
```

```bash
az login
```

### `KUBESPIN_REGISTRY_DSN`, on nearly every command

`apply`, `delete`, and every `fleet` subcommand read the Fleet Registry
(a Postgres database), and its DSN has **no default and no flag** on purpose
(see [Fleet bootstrap troubleshooting](fleet-bootstrap.md#troubleshooting)) —
a flag would leak the password into shell history and process listings.
Supply it as `KUBESPIN_REGISTRY_DSN`, or as `registry-dsn` in the config file:

```bash
export KUBESPIN_REGISTRY_DSN=postgres://user:pass@host:5432/dbname?sslmode=require
```

`fleet bootstrap` additionally takes its own `--region` flag — the AWS region
for the ingestion Lambda/IAM/API Gateway it provisions, unrelated to the
registry DSN.

### `--profile`, on `apply` and `delete`

Both commands build and validate a full `ClusterSpec`, and a spec without a
`name@version` profile reference is rejected before any cloud call. Either
pass `--profile tier-small@1.0.0` or supply a `--spec` file whose `profile:`
block is filled in. The builtin catalog ships `tier-small@1.0.0`,
`tier-standard@1.0.0`, and `tier-regulated@1.0.0`.

### GitHub, on everything that touches a cluster repository

Real (non-dry-run) `apply`, and every `delete`, `fleet update`, and
`fleet audit`, create or read cluster repositories. Each needs both of these,
from [`.env.example`](https://github.com/GitOpsHub/kubespin/blob/main/.env.example):

- **`GITHUB_TOKEN`** — a token with repo-create/push scope, read from the
  environment (never a flag, so it never lands in shell history).
- **`--github-org`** — the org cluster repositories live in. `GITHUB_ORG` in
  `.env.example` is a reminder to set this, not something kubespin reads
  directly: export it and pass it yourself, e.g. `--github-org "$GITHUB_ORG"`.

```bash
export GITHUB_TOKEN=ghp_...
```

```bash
export GITHUB_ORG=GitOpsHub
```

An `apply --dry-run` is the one exception: it only reads the Fleet Registry
and returns before any repository client is built, so it needs neither.

## Auth workflows

```bash
# Log in to every configured provider (AWS, GCP, Azure)
kubespin login
```

```bash
# Only the providers you need right now
kubespin login --only aws,gcp
```

```bash
# Force re-authentication even if the cached session still looks valid
kubespin login --force
```

```bash
# Check session state without changing anything — useful when a provisioner
# fails and you're not sure if it's a bug or an expired session
kubespin status
```

```bash
# Clear a cached session
kubespin logout --only azure
```

`status` never fails the command on an unauthenticated provider: reporting
that is exactly what it exists for.

## Spin up a single cluster

Each of these is self-contained: authenticate, apply, confirm it landed.

### AWS, private cluster

kubespin creates the VPC, two subnets across two AZs, an Internet Gateway,
and a route table, because `--subnets` is omitted.

```bash
kubespin login --only aws

kubespin apply \
  --provider aws \
  --region us-east-1 \
  --cluster-id demo-aws \
  --access private \
  --profile tier-small@1.0.0 \
  --github-org "$GITHUB_ORG"

kubespin fleet status --phase ready
```

### GCP, public cluster with a larger node pool

```bash
kubespin login --only gcp

kubespin apply \
  --provider gcp \
  --gcp-project kubernetes-dev-502710 \
  --region us-central1 \
  --cluster-id demo-gcp \
  --access public \
  --profile tier-small@1.0.0 \
  --instance-type e2-standard-4 \
  --min-size 2 --max-size 6 --desired-size 3 \
  --github-org "$GITHUB_ORG"

kubespin fleet status --phase ready
```

`--min-size`, `--max-size`, and `--desired-size` describe the single
`default` node pool built from flags. Richer topologies belong in a
`--spec` file.

#### Quota on low-quota / sandbox GCP projects

GKE treats a region (as opposed to a zone) `--region` as a **regional
cluster**, which replicates the default node pool across 3 zones. Quota is
consumed per zone, not per cluster:

- **CPU**: `--desired-size 3` at `e2-standard-4` is `3 nodes × 4 vCPU × 3
  zones = 36 vCPU`, not the 12 vCPU it looks like at a glance.
- **Disk**: GKE's node boot disk defaults to a **fixed 100Gi regardless of
  machine type** — `--instance-type` does not change it. Even
  `--desired-size 1` is 1 node per zone, so the minimum footprint of a
  regional cluster is `3 × 100Gi = 300Gi`, which alone exceeds a common
  250Gi `SSD_TOTAL_GB` sandbox quota.

Use `--disk-size` (added alongside `--instance-type`/`--min-size`/
`--max-size`/`--desired-size`) to bring the disk footprint down explicitly:

```bash
kubespin apply \
  --provider gcp \
  --gcp-project kubernetes-dev-502710 \
  --region us-central1 \
  --cluster-id demo-gcp \
  --access private \
  --profile tier-small@1.0.0 \
  --instance-type e2-standard-2 \
  --min-size 1 --max-size 3 --desired-size 1 \
  --disk-size 30 \
  --github-org "$GITHUB_ORG"
```

That's 6 vCPU and 90Gi of boot disk total — comfortably under a 12 vCPU /
250Gi sandbox quota. Otherwise, request a `CPUS_ALL_REGIONS` and
`SSD_TOTAL_GB` quota increase for the target region before applying at the
larger footprint above.

### Azure, on a subnet you already own

Passing `--subnets` tells kubespin the network is yours: it is used unchanged
and nothing about it is created or modified.

```bash
kubespin login --only azure

kubespin apply \
  --provider azure \
  --azure-subscription "$AZURE_SUBSCRIPTION_ID" \
  --region eastus \
  --cluster-id demo-azure \
  --access private \
  --profile tier-standard@1.0.0 \
  --instance-type Standard_D4s_v7 \
  --profiles-repo platform-profiles \
  --subnets "/subscriptions/$AZURE_SUBSCRIPTION_ID/resourceGroups/my-rg/providers/Microsoft.Network/virtualNetworks/my-vnet/subnets/my-subnet" \
  --github-org "$GITHUB_ORG"

kubespin fleet status --phase ready
```

`--profile` with no `--profiles-repo` resolves against the builtin catalog —
useful before `platform-profiles` exists yet. Drop `--subnets` and kubespin
creates the resource group, VNet, and subnet itself.

Pin `--instance-type` explicitly on repeat `apply` runs against an existing
cluster: leaving it unset falls back to a per-provider default baked into the
kubespin binary, and if that default changes between versions, the next
`apply` tries to drift the node pool onto the new value. AKS (and the other
clouds) reject changing an existing pool's instance type in place, so an
unpinned default that moves out from under a live cluster turns an
idempotent `apply` into a hard failure.

### Telling clusters where to push status

The in-cluster reporter needs egress to the Central Ingestion API, and the
allowlist rule is provisioned during cluster creation. Pass the host that
`fleet bootstrap` printed:

```bash
kubespin apply \
  --provider aws \
  --region us-east-1 \
  --cluster-id demo-aws \
  --access private \
  --profile tier-small@1.0.0 \
  --ingestion-endpoint abc123.execute-api.us-east-1.amazonaws.com \
  --github-org "$GITHUB_ORG"
```

### From a cluster.yaml instead of flags

The file is the same `cluster.yaml` a cluster's repository holds, so what you
pass here is what gets committed. Unknown keys are rejected rather than
silently ignored.

```yaml
# cluster.yaml
id: demo-aws
provider: aws
region: us-east-1
access: private
profile:
  name: tier-small
  version: 1.0.0
nodePools:
  - name: default
    instanceType: m6i.large
    minSize: 1
    maxSize: 5
    desiredSize: 2
    diskSizeGB: 30 # optional; 0 or omitted uses the cloud default
subnets: []
```

```bash
kubespin apply --spec ./cluster.yaml \
  --github-org "$GITHUB_ORG"
```

An explicitly-set flag overrides the file, so a checked-out spec can be
reused with one field changed:

```bash
kubespin apply --spec ./cluster.yaml --cluster-id demo-aws-2 \
  --github-org "$GITHUB_ORG"
```

Overridable this way: `--cluster-id`, `--provider`, `--region`, `--access`,
`--kubernetes-version`, `--profile`, `--subnets`, and the three CIDR flags.
The node pool flags (`--instance-type`, `--min-size`, `--max-size`,
`--desired-size`, `--disk-size`) are **not** — they only build the single
`default` pool when the spec has no `nodePools` at all, so a file's pools
are never partially overwritten from the command line. Edit the file to
resize a pool or change its disk size.

### Preview before applying

An `apply --dry-run` reads the Fleet Registry and reports the phase a real
run would resume from. It never touches the cluster's own cloud, and never
builds a GitHub client — so it needs neither `GITHUB_TOKEN` nor
`--github-org`:

```bash
kubespin apply \
  --provider aws \
  --region us-east-1 \
  --cluster-id demo-aws \
  --access private \
  --profile tier-small@1.0.0 \
  --dry-run
```

On an unregistered cluster it prints:

```
cluster demo-aws is not registered; apply would create it from phase pending
```

## Smoke test: create and destroy a throwaway cluster

The cheapest way to validate a kubespin install (a fresh Fleet Registry, a
new environment, after upgrading) end to end: bring up one real cluster per
cloud with the smallest footprint, confirm it reaches `ready`, then tear it
down.

`apply` installs Argo CD by connecting to the cluster's API server directly
from wherever `apply` runs (see [Architecture](architecture.md)), so
`--access private` only works if that machine already has network reachability
into the cluster's VPC/VNet. Running this from a laptop or a CI runner without
VPN/peering needs `--access public --authorized-cidrs <your IP>/32` instead —
on GCP that flag is required outright, since GKE's master-authorized-networks
otherwise has an empty allowlist and refuses everyone, including the operator.
`--spot` picks the cheapest viable instance type/pool size for each cloud
(see [Low-cost dev clusters](low-cost-dev-clusters.md)).

```bash
MY_IP=$(curl -s https://checkip.amazonaws.com)

kubespin login --only aws,gcp

kubespin apply \
  --provider aws \
  --region us-east-1 \
  --cluster-id smoke-test-aws \
  --access public \
  --authorized-cidrs "$MY_IP/32" \
  --profile tier-small@1.0.0 \
  --spot \
  --github-org "$GITHUB_ORG"

kubespin apply \
  --provider gcp \
  --gcp-project kubernetes-dev-502710 \
  --region us-central1 \
  --cluster-id smoke-test-gcp \
  --access public \
  --authorized-cidrs "$MY_IP/32" \
  --profile tier-small@1.0.0 \
  --spot \
  --github-org "$GITHUB_ORG"

kubespin fleet status --phase ready
```

Once both clusters show `ready`, tear them down:

```bash
kubespin delete --provider aws --region us-east-1 --cluster-id smoke-test-aws \
  --profile tier-small@1.0.0 --github-org "$GITHUB_ORG" --yes

kubespin delete --provider gcp --gcp-project kubernetes-dev-502710 --region us-central1 \
  --cluster-id smoke-test-gcp --profile tier-small@1.0.0 --github-org "$GITHUB_ORG" --yes
```

A GCP project with several prior test clusters can hit the account-level
`NETWORKS` quota (5 VPCs by default) before kubespin ever gets a chance to
create one for the new cluster — the error surfaces as `Quota 'NETWORKS'
exceeded` from `create cluster: ensuring network`. Check
`gcloud compute networks list` for orphaned `kubespin-*` networks left behind
by earlier runs (no matching entry in `kubespin fleet status`, no attached
GKE cluster in `gcloud container clusters list`) before requesting a quota
increase — deleting one frees a slot immediately.

## Fleet lifecycle

The shared fleet infrastructure — the Central Ingestion API — is provisioned
once per fleet account, before any cluster, via `fleet bootstrap`. The Fleet
Registry itself is a separately operated Postgres database
(`KUBESPIN_REGISTRY_DSN`); it self-migrates its schema on first connect, so
there is nothing to provision for it. Full walkthrough, including required
IAM permissions, in [Fleet bootstrap](fleet-bootstrap.md).

```bash
# 1. Build the ingestion handler — bootstrap reads it from disk
make lambda

# 2. Preview
kubespin fleet bootstrap --account-id 465532803838 --region us-east-1 --dry-run

# 3. Apply
kubespin fleet bootstrap --account-id 465532803838 --region us-east-1

# 4. Re-run the preview: everything must now report in sync
kubespin fleet bootstrap --account-id 465532803838 --region us-east-1 --dry-run
```

```bash
# 5. Spin up clusters (repeat per cluster; see "Spin up a single cluster")
kubespin apply --provider aws --region us-east-1 --cluster-id demo-aws \
  --access private --profile tier-small@1.0.0 \
  --github-org "$GITHUB_ORG"
```

```bash
# 6. Watch the fleet — read-only, never connects to a cluster
kubespin fleet status
kubespin fleet status --stale-only --stale-threshold 30m
kubespin fleet status --output json
kubespin fleet status --provider aws --phase ready

# Same data, rendered as a static HTML snapshot you can open in a browser
kubespin fleet dashboard
```

```bash
# 7. Roll a component version across every matching cluster
kubespin fleet update --component argo-cd --version 2.11.0 --concurrency 8 \
  --github-org "$GITHUB_ORG"

# Scope a wave to one cloud
kubespin fleet update --component cert-manager --version 1.15.1 --provider aws \
  --github-org "$GITHUB_ORG"
```

```bash
# 8. Check live infra against each cluster's cluster.yaml
kubespin fleet audit \
  --github-org "$GITHUB_ORG"

kubespin fleet audit --provider gcp --concurrency 8 \
  --gcp-project kubernetes-dev-502710 \
  --github-org "$GITHUB_ORG"
```

`fleet audit` describes live infrastructure through each cloud's SDK, so a
fleet containing GCP or Azure clusters needs `--gcp-project` /
`--azure-subscription` even when they are not the audit's focus — without
them, those clusters report `FAILED` rather than being skipped.

## Tear down

Repositories are archived, never deleted — see
[Architecture](architecture.md) for why delete is a reverse teardown rather
than the inverse of a Terraform destroy.

```bash
# Interactive: prompts to type the cluster ID to confirm
kubespin delete \
  --provider aws \
  --region us-east-1 \
  --cluster-id demo-aws \
  --profile tier-small@1.0.0 \
  --github-org "$GITHUB_ORG"
```

```bash
# Scripted: skip the confirmation prompt
kubespin delete \
  --provider gcp \
  --gcp-project kubernetes-dev-502710 \
  --region us-central1 \
  --cluster-id demo-gcp \
  --profile tier-small@1.0.0 \
  --github-org "$GITHUB_ORG" \
  --yes
```

```bash
# Using the same cluster.yaml apply was run with
kubespin delete --spec ./cluster.yaml \
  --github-org "$GITHUB_ORG" --yes
```

`delete` validates a full spec exactly like `apply`, which is why
`--profile` appears here too even though teardown never resolves addons.
Several other flags (`--instance-type`, `--min-size`, `--max-size`,
`--desired-size`, `--disk-size`, `--kubernetes-version`, the CIDR flags) are
accepted for spec compatibility and ignored.

There is no fleet-infrastructure teardown command, deliberately — see
[Fleet bootstrap: re-running, resuming, and tearing down](fleet-bootstrap.md#re-running-resuming-and-tearing-down).

## Which commands honour `--dry-run`

`--dry-run` is a root persistent flag, so every command *accepts* it, but only
two act on it:

| Command | `--dry-run` |
|---|---|
| `fleet bootstrap` | **Honoured.** `Plan` is strictly read-only; the test fakes fail the build if a dry run makes a mutating call. |
| `apply` | **Honoured.** Reads the Fleet Registry and reports the phase a run would resume from; touches no cloud and no repository. |
| `delete` | **Ignored.** The teardown runs. |
| `fleet update` | **Ignored.** The wave commits. |
| `fleet audit` | Not applicable — read-only by construction. |
| `fleet status` | Not applicable — read-only by construction. |
| `fleet dashboard` | Not applicable — read-only by construction. |

Passing `--dry-run` still logs `dry run: no changes will be made` on every
command, because that line is emitted by the shared root pre-run. On `delete`
and `fleet update` it does not reflect what the command then does.

## Global flags and configuration

Precedence is **flags > `KUBESPIN_*` environment variables > config file >
defaults**.

```bash
kubespin fleet status \
  --log-level debug --log-format json
```

Logs go to stderr and command output to stdout, so the two can be separated:

```bash
kubespin fleet status --output json 2>/dev/null
```

A config file at `$XDG_CONFIG_HOME/kubespin/config.yaml` or `./config.yaml`
(or wherever `--config` points) removes the repeated flags entirely:

```yaml
log-level: info
log-format: text
registry-dsn: postgres://user:pass@host:5432/dbname?sslmode=require
```

With that in place, or with `KUBESPIN_REGISTRY_DSN` exported, every example
above works as written — the DSN is never a flag.

## Exit codes

`0` on success, `1` on any failure. Failures are printed to stderr prefixed
with `kubespin:`.
