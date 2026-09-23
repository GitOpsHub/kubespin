# Examples

Working commands to copy, paste, and adjust. For flag-by-flag detail see the
[CLI reference](cli/kubespin.md); for *why* the system behaves this way see
[Architecture](architecture.md).

Every command below is written as `kubespin`, run from the root of a
repository checkout after `make build`. Nothing here is a sketch: each
example carries every flag the command actually requires, so it runs as
written once the prerequisites below are in place.

```bash
make build
```

## Quickstart

The fastest path to one running cluster:

There is nothing to set up first beyond the Postgres registry
`KUBESPIN_REGISTRY_DSN` points at, which migrates its own schema on first
connect.

```bash
kubespin login --only aws

kubespin apply \
  --provider aws \
  --region us-east-1 \
  --cluster-id demo-aws \
  --access private \
  --github-org "$GITHUB_ORG"

```

That's the same AWS example as [Spin up a single cluster](#aws-private-cluster)
below, with `GITHUB_TOKEN`, `GITHUB_ORG`, and `KUBESPIN_REGISTRY_DSN` assumed
set — see [Prerequisites](#prerequisites) for what those are and why each is
required. For the cheapest possible cluster to try this with, see
[Low-cost dev clusters](low-cost-dev-clusters.md). For fully-managed compute
without managing node pools (GKE Autopilot or EKS Auto Mode), see
[Autopilot clusters](autopilot-clusters.md).

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

### `KUBESPIN_REGISTRY_DSN`, on apply and delete

`apply` and `delete` read the cluster registry (a Postgres database), and its
DSN has **no default and no flag** on purpose — a flag would leak the password
into shell history and process listings.
Supply it as `KUBESPIN_REGISTRY_DSN`, or as `registry-dsn` in the config file:

```bash
export KUBESPIN_REGISTRY_DSN=postgres://user:pass@host:5432/dbname?sslmode=require
```

### `--size`, on `apply` and `delete`

Both commands build and validate a full `ClusterSpec`. `--size` picks the
cluster's addon footprint from the builtin catalog — `small`, `medium`, or
`large` — and defaults to `small` when omitted, so no flag is required for
the common case. Argo CD and `cluster-autoscaler` (configured for each
cloud) ship at every size; `medium` adds
Velero + Falco, `large` adds strict Kyverno policies + audit logging + OTel.

### GitHub, on everything that touches a cluster repository

Real (non-dry-run) `apply` and every `delete` create or read cluster
repositories. Each needs both of these,
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

An `apply --dry-run` is the one exception: it only reads the cluster registry
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
  --github-org "$GITHUB_ORG"

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
  --instance-type e2-standard-4 \
  --min-size 2 --max-size 6 --desired-size 3 \
  --github-org "$GITHUB_ORG"

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
  --size medium \
  --instance-type Standard_D4s_v7 \
  --subnets "/subscriptions/$AZURE_SUBSCRIPTION_ID/resourceGroups/my-rg/providers/Microsoft.Network/virtualNetworks/my-vnet/subnets/my-subnet" \
  --github-org "$GITHUB_ORG"

```

`--size medium` resolves against the builtin catalog — small, medium, and
large are all fully code-defined, no external repo involved. Drop `--subnets`
and kubespin creates the resource group, VNet, and subnet itself.

Pin `--instance-type` explicitly on repeat `apply` runs against an existing
cluster: leaving it unset falls back to a per-provider default baked into the
kubespin binary, and if that default changes between versions, the next
`apply` tries to drift the node pool onto the new value. AKS (and the other
clouds) reject changing an existing pool's instance type in place, so an
unpinned default that moves out from under a live cluster turns an
idempotent `apply` into a hard failure.

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
size: small
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
`--kubernetes-version`, `--size`, `--subnets`, and the three CIDR flags.
The node pool flags (`--instance-type`, `--min-size`, `--max-size`,
`--desired-size`, `--disk-size`) are **not** — they only build the single
`default` pool when the spec has no `nodePools` at all, so a file's pools
are never partially overwritten from the command line. Edit the file to
resize a pool or change its disk size.

### Per-cluster override patch

`--size` picks a cluster's addon set from the builtin catalog, but one
cluster sometimes needs to deviate from it — pin a different chart version,
tweak one addon's Helm values, or drop an addon it doesn't want. That's what
`overrides:` in `cluster.yaml` is for (`core.AddonOverride`,
[`internal/catalog.Merge`](reference/catalog.md#merge)). It patches addons
the size already carries — it never introduces a new one, and a typo in the
name fails at `apply` time rather than being silently ignored:

```yaml
# cluster.yaml
id: demo-aws
provider: aws
region: us-east-1
access: private
size: medium
nodePools:
  - name: default
    instanceType: m6i.large
    minSize: 1
    maxSize: 5
    desiredSize: 2
subnets: []
overrides:
  # Re-pin one addon to a version newer than the catalog's, e.g. to pick up
  # a fix before the next kubespin release ships it.
  - name: cert-manager
    version: "1.16.2"
  # Patch one addon's Helm values — merged one level deep onto the
  # catalog's own values, so keys you don't mention are left alone.
  - name: kube-prometheus-stack
    values:
      grafana:
        adminPassword: "changeme"
  # Drop an addon entirely, e.g. because this cluster already runs its own
  # ExternalDNS and doesn't want the catalog's.
  - name: external-dns
    disable: true
```

```bash
kubespin apply --spec ./cluster.yaml \
  --github-org "$GITHUB_ORG"
```

This is the mechanism for "extra Helm deployments this one cluster needs" —
it lives in the cluster's own `cluster.yaml`, survives every subsequent
`apply` (which re-renders `addons.yaml` from size + overrides
on every run), and requires no external repository. Naming an addon the
cluster's size doesn't carry (e.g. `velero` on a `small` cluster, which only
`medium` and up include) fails validation with `ErrUnknownOverride`.

### Preview before applying

An `apply --dry-run` reads the cluster registry and reports the phase a real
run would resume from. It never touches the cluster's own cloud, and never
builds a GitHub client — so it needs neither `GITHUB_TOKEN` nor
`--github-org`:

```bash
kubespin apply \
  --provider aws \
  --region us-east-1 \
  --cluster-id demo-aws \
  --access private \
  --dry-run
```

On an unregistered cluster it prints:

```
cluster demo-aws is not registered; apply would create it from phase pending
```

## Smoke test: create and destroy a throwaway cluster

The cheapest way to validate a kubespin install (a fresh registry, a
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
  --spot \
  --github-org "$GITHUB_ORG"

kubespin apply \
  --provider gcp \
  --gcp-project kubernetes-dev-502710 \
  --region us-central1 \
  --cluster-id smoke-test-gcp \
  --access public \
  --authorized-cidrs "$MY_IP/32" \
  --spot \
  --github-org "$GITHUB_ORG"

```

Once both clusters show `ready`, tear them down:

```bash
kubespin delete --provider aws --region us-east-1 --cluster-id smoke-test-aws \
  --github-org "$GITHUB_ORG" --yes

kubespin delete --provider gcp --gcp-project kubernetes-dev-502710 --region us-central1 \
  --cluster-id smoke-test-gcp --github-org "$GITHUB_ORG" --yes
```

A GCP project with several prior test clusters can hit the account-level
`NETWORKS` quota (5 VPCs by default) before kubespin ever gets a chance to
create one for the new cluster — the error surfaces as `Quota 'NETWORKS'
exceeded` from `create cluster: ensuring network`. Check
`gcloud compute networks list` for orphaned `kubespin-*` networks left behind
by earlier runs (no attached GKE cluster in
`gcloud container clusters list`) before requesting a quota
increase — deleting one frees a slot immediately.

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
  --github-org "$GITHUB_ORG"
```

```bash
# Scripted: skip the confirmation prompt
kubespin delete \
  --provider gcp \
  --gcp-project kubernetes-dev-502710 \
  --region us-central1 \
  --cluster-id demo-gcp \
  --github-org "$GITHUB_ORG" \
  --yes
```

```bash
# Using the same cluster.yaml apply was run with
kubespin delete --spec ./cluster.yaml \
  --github-org "$GITHUB_ORG" --yes
```

`delete` validates a full spec exactly like `apply`, which is why `--size`
is accepted here too (defaulting to `small`) even though teardown never
resolves addons.
Several other flags (`--instance-type`, `--min-size`, `--max-size`,
`--desired-size`, `--disk-size`, `--kubernetes-version`, the CIDR flags) are
accepted for spec compatibility and ignored.

## Which commands honour `--dry-run`

`--dry-run` is a root persistent flag, so every command *accepts* it, but only
one acts on it:

| Command | `--dry-run` |
|---|---|
| `apply` | **Honoured.** Reads the cluster registry and reports the phase a run would resume from; touches no cloud and no repository. |
| `delete` | **Ignored.** The teardown runs. |

Passing `--dry-run` still logs `dry run: no changes will be made` on every
command, because that line is emitted by the shared root pre-run. On `delete`
it does not reflect what the command then does.

## Global flags and configuration

Precedence is **flags > `KUBESPIN_*` environment variables > config file >
defaults**.

```bash
kubespin status \
  --log-level debug --log-format json
```

Logs go to stderr and command output to stdout, so the two can be separated:

```bash
kubespin status 2>/dev/null
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
