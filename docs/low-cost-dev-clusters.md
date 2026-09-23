# Low-cost dev clusters

One flag for the cheapest dev/learning cluster kubespin can build on a given
cloud: `--spot`. It is opt-in — omitting it leaves `apply`'s default behavior
(on-demand, larger instance types, HA where the cloud offers it, private
nodes) unchanged.

## `--spot`

```bash
kubespin apply \
  --provider aws \
  --region us-east-1 \
  --cluster-id dev-aws \
  --access private \
  --spot \
  --github-org "$GITHUB_ORG"
```

No `--instance-type`/`--min-size`/`--max-size`/`--desired-size`/`--disk-size`
needed — `--spot` picks smaller defaults for all five, sized to still run
kubespin's default (`--size small`) addon set (kube-prometheus-stack, ingress-nginx,
kyverno, and the rest) without failing to schedule:

| Cloud | Instance type | min/max/desired | Disk |
|---|---|---|---|
| AWS | `auto` (cheapest spot types, see below) | 1/2/1 | 20 GB |
| GCP | `e2-medium` | 1/2/1 | 30 GB |
| Azure | `Standard_B2s` | 1/2/1 | 30 GB |

On AWS the instance type is `auto`, not a fixed type. When kubespin creates
the node group, it looks up the current spot price of every current-generation
x86_64 type with 2 vCPUs and at least 4 GiB of memory that is offered in all
of the cluster's subnet zones. It then gives EKS the six cheapest, ranked by
each type's highest price across those zones. Typical picks are `t3.medium`,
`t3a.medium`, and `c6i.large`. EKS's spot allocation (`price-capacity-optimized`)
then launches each node from the cheapest of those pools that has capacity,
so a reclaimed or empty pool falls back to the next-cheapest type instead of
failing to launch. The list is chosen once, when the node group is created;
EKS can't change a node group's instance types afterward. To pick up newer
prices, delete and recreate the node pool. `auto` only works for AWS spot
pools, because a GKE node pool or an AKS agent pool takes a single machine
size.

On AWS, kubespin also turns on VPC CNI prefix delegation (see
`docs/reference/provisioner-aws.md`). That lets each node run up to 110 pods
instead of the 17 a `t3.medium` gets by default, so a node fills up on CPU
and memory rather than on pod slots. The cluster then needs fewer nodes,
which is what keeps a spot cluster at its minimum size.

These are not each cloud's absolute cheapest instance (a free-tier
`t3.micro`/`e2-micro`/`B1S`, at ~1 vCPU/1GB, is too small to run this addon
set reliably), just the smallest ones that reliably will. Pass
`--instance-type`/`--min-size`/`--max-size`/`--desired-size`/`--disk-size`
explicitly to override any one of them without losing the rest.

`--spot` also requests spot/preemptible capacity for the node pool instead of
on-demand. Nodes can be reclaimed by the cloud provider with little notice —
fine for learning, wrong for anything that needs to stay up.

- **AWS (EKS)** and **GCP (GKE)**: honored, sets the node group/pool to spot
  capacity.
- **Azure (AKS)**: a no-op for capacity type. AKS requires its default/first
  agent pool to run in System mode, and System-mode pools cannot use Spot
  priority (they may need to host system pods at any time, which a pool
  subject to eviction can't guarantee). The smaller `Standard_B2s` default
  above is where Azure's savings come from instead.

## `--spot` on GCP: also goes zonal and skips Cloud NAT

GKE clusters are **regional** by default: the control plane is replicated
across 3 zones (~$0.10/hr, never eligible for GCP's free tier), and nodes are
always private, which requires an always-on Cloud Router + Cloud NAT
(~$0.045/hr + data processing) just to reach the internet.

Passing `--spot` with `--provider gcp` defaults to also:

- **Zonal cluster** (`--zone` implied as `<region>-a`): a single-zone control
  plane instead of a 3-zone regional one. This is what makes the cluster
  eligible for GCP's one-free-zonal-cluster-per-billing-account tier.
  Trade-off: no HA control plane — fine for a dev cluster, wrong for
  production.
- **Public nodes** (`--gcp-public-nodes` implied): nodes get public IPs
  instead of GKE provisioning a Cloud Router + Cloud NAT for them. Trade-off:
  less network isolation.

```bash
kubespin apply \
  --provider gcp \
  --gcp-project kubernetes-dev-502710 \
  --region us-central1 \
  --cluster-id dev-gcp \
  --access private \
  --spot \
  --github-org "$GITHUB_ORG"
```

Override either piece individually if you want spot without going zonal, or
zonal without spot:

```bash
# Zonal, on-demand (no --spot)
kubespin apply \
  --provider gcp \
  --gcp-project kubernetes-dev-502710 \
  --region us-central1 \
  --cluster-id dev-gcp \
  --access private \
  --zone us-central1-a \
  --github-org "$GITHUB_ORG"
```

```bash
# Regional, spot, but keep Cloud NAT (opt back into private nodes)
kubespin apply \
  --provider gcp \
  --gcp-project kubernetes-dev-502710 \
  --region us-central1 \
  --cluster-id dev-gcp \
  --access private \
  --spot --gcp-public-nodes=false \
  --github-org "$GITHUB_ORG"
```

## Fixed costs `--spot` cannot remove

- **AWS EKS control plane**: a flat ~$0.10/hr, with no free tier and no
  way around it — kubespin's AWS networking already avoids a NAT Gateway
  entirely (public subnets with auto-assigned public IPs), which is the
  other major EKS cost driver.
- **Azure AKS**: every cluster kubespin creates explicitly requests the
  **Free** SKU tier (no control-plane charge), regardless of `--spot`.
- **GCP Cloud NAT**, if you don't pass `--spot`/`--gcp-public-nodes`: an
  unavoidable fixed cost of GKE's default private-node configuration.

## Putting it together: cheapest dev cluster per cloud

```bash
# AWS
kubespin apply --provider aws --region us-east-1 --cluster-id dev-aws \
  --access private \
  --spot --github-org "$GITHUB_ORG"

# GCP — also goes zonal + public-nodes automatically, see above
kubespin apply --provider gcp --gcp-project kubernetes-dev-502710 \
  --region us-central1 --cluster-id dev-gcp \
  --access private \
  --spot --github-org "$GITHUB_ORG"

# Azure — --spot still sizes the node pool down even though capacity stays on-demand
kubespin apply --provider azure --azure-subscription "$AZURE_SUBSCRIPTION_ID" \
  --region eastus --cluster-id dev-azure \
  --access private \
  --spot --github-org "$GITHUB_ORG"
```

See [Examples](examples.md) for the full flag reference on `apply`, and
[Examples: quota on low-quota / sandbox GCP projects](examples.md#quota-on-low-quota--sandbox-gcp-projects)
for why `--disk-size` also matters on a regional (non-`--spot`) GCP cluster.

## `make spot`: all three clouds at once

`make spot` runs this same recipe on AWS, GCP, and Azure in parallel, with
`--access public --authorized-cidrs` pinned to the caller's own public IP
(auto-detected) instead of the `--access private` shown above — a bare `make
spot` has no VPN/bastion reachability into any of the three VPCs/VNets to
assume, and public-plus-authorized-cidrs is what lets the Argo CD install
step in each `apply` reach that cluster's API server from this machine.

```bash
export GITHUB_ORG=GitOpsHub
export GCP_PROJECT=kubernetes-dev-502710
export AZURE_SUBSCRIPTION_ID=3df9adbd-ea55-4c92-964c-0252031979de
make spot
```

Requires `kubespin login` to have already authenticated all three clouds
(`kubespin status` to check), `GITHUB_TOKEN`/`KUBESPIN_REGISTRY_DSN` set the
same as any real `apply`, and the three env vars above. Cluster IDs and
regions default to `eks-spot-dev`/`us-east-1`, `gke-spot-dev`/`us-central1`,
and `aks-spot-dev`/`eastus`, each overridable, e.g.:

```bash
make spot AWS_REGION=us-west-2 AWS_CLUSTER_ID=my-aws-dev
```

To run on only some clouds, name them as extra goals. Only the env vars
those clouds need are checked, so `make spot aws` needs no `GCP_PROJECT` or
`AZURE_SUBSCRIPTION_ID`:

```bash
make spot aws              # just the EKS spot cluster
make spot gcp azure        # GKE + AKS
make destroy-spot aws      # tear the EKS one down again
```

`make autopilot` and `make destroy` accept the same cloud goals.

## Manual multi-cloud smoke test

`make spot` is the one-shot version. Running the same idea by hand — one
`apply` per cloud, then tearing both down — is useful
when you want to watch each step. See
[Examples: smoke test](examples.md#smoke-test-create-and-destroy-a-throwaway-cluster)
for the full walkthrough (it already uses `--spot`); the only difference here
is which cluster IDs and clouds you pick.
