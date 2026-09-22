# Autopilot clusters

One flag, `--autopilot`, swaps standard node-pool provisioning for each
provider's fully-managed compute mode: **GKE Autopilot** on GCP and **EKS
Auto Mode** on AWS. It is opt-in — omitting it leaves `apply`'s default
behavior (manually-sized node pools, `cluster-autoscaler`/Karpenter as an
addon) unchanged. **Azure has no equivalent today** (AKS's "Automatic"/Node
Autoprovisioning mode requires an unreleased beta SDK); `--autopilot` is
simply unsupported on `--provider azure`.

Argo CD is installed and the cluster is registered exactly the same way as
any other `apply` — Autopilot/Auto Mode only change how compute is
provisioned, not the rest of the `pending → ... → ready` flow.

## GCP: GKE Autopilot

```bash
kubespin apply \
  --provider gcp \
  --gcp-project kubernetes-dev-502710 \
  --region us-central1 \
  --cluster-id gke-autopilot-01 \
  --access private \
  --autopilot \
  --github-org "$GITHUB_ORG"
```

Public access (GCP requires `--authorized-cidrs` to reach the endpoint at
all under `--access public`):

```bash
kubespin apply \
  --provider gcp \
  --gcp-project kubernetes-dev-502710 \
  --region us-central1 \
  --cluster-id gke-autopilot-01 \
  --access public \
  --authorized-cidrs "$(curl -s ifconfig.me)/32" \
  --autopilot \
  --github-org "$GITHUB_ORG"
```

GKE manages node pools, node autoscaling, and node OS/security patching
itself. An Autopilot cluster's resolved addon set carries **only Argo CD** —
`--size` has no effect on the addon set under `--autopilot` (it still exists
as a flag, since it is meaningless to reject outright, but nothing beyond
Argo CD is delivered).

`--access`/`--authorized-cidrs`/`--gcp-public-nodes`/`--subnets` all behave
the same as on a standard GKE cluster — Autopilot only changes compute
management, not the control plane's network exposure.

## AWS: EKS Auto Mode

```bash
kubespin apply \
  --provider aws \
  --region us-east-1 \
  --cluster-id eks-auto-01 \
  --access private \
  --autopilot \
  --github-org "$GITHUB_ORG"
```

Public access (restricts the otherwise-open `0.0.0.0/0` default to the
caller's own IP):

```bash
kubespin apply \
  --provider aws \
  --region us-east-1 \
  --cluster-id eks-auto-01 \
  --access public \
  --authorized-cidrs "$(curl -s ifconfig.me)/32" \
  --autopilot \
  --github-org "$GITHUB_ORG"
```

EKS Auto Mode provisions compute, block storage, and load balancing itself.
As with GKE Autopilot, kubespin's resolved Helm addon set carries only Argo
CD. Two EKS-managed addons remain outside that Helm-based addon set and are
still handled directly by the provisioner: `aws-ebs-csi-driver` is skipped
(Auto Mode's built-in block storage replaces it) and `aws-efs-csi-driver` is
still installed, since EFS has no Auto Mode equivalent.

## What `--autopilot` changes under the hood

- **No node-pool flags.** `--instance-type`, `--min-size`, `--max-size`,
  `--desired-size`, `--disk-size`, and `--spot` are all rejected if passed
  explicitly alongside `--autopilot` — the provider manages compute itself,
  so kubespin treats silently discarding one of these as more likely to
  surprise an operator than erroring outright.
- **Only Argo CD in `addons.yaml`.** Every catalog Helm addon (cert-manager,
  monitoring, ingress, Kyverno, `cluster-autoscaler`/`karpenter`, and so on)
  is dropped for an Autopilot/Auto Mode cluster — Argo CD is installed and
  the cluster registers, but nothing else syncs from the cluster repo unless
  a per-cluster override patch adds it back.
- **IAM (AWS only).** EKS Auto Mode needs a dedicated node role, distinct
  from the role manually-managed node groups use, with
  `AmazonEKSWorkerNodeMinimalPolicy` + `AmazonEC2ContainerRegistryPullOnly`
  attached (there is no single "Auto Mode node policy" — AWS documents this
  exact pair). The cluster role gets four extra managed policies beyond the
  standard `AmazonEKSClusterPolicy`: `AmazonEKSComputePolicy`,
  `AmazonEKSBlockStoragePolicyV2` (note the `V2` suffix —
  `AmazonEKSBlockStoragePolicy` without it does not exist),
  `AmazonEKSLoadBalancingPolicy`, and `AmazonEKSNetworkingPolicy`; its trust
  policy also grants `sts:TagSession` (not just `sts:AssumeRole`), which Auto
  Mode's tag-based resource scoping requires. Kubespin provisions and tears
  all of this down automatically; there is nothing extra to configure.

## Teardown

Delete works identically to any other cluster — **`--autopilot` is not
needed on delete.** Kubespin detects Autopilot/Auto Mode from the live
cluster itself (GKE's `Autopilot.Enabled`, EKS's `ComputeConfig.Enabled`)
rather than trusting the flags a `delete` invocation happens to pass, since
`delete` reconstructs its spec from flags with no `cluster.yaml` to read
`autopilot: true` back from:

```bash
kubespin delete --provider gcp --gcp-project kubernetes-dev-502710 \
  --region us-central1 --cluster-id gke-autopilot-01

kubespin delete --provider aws --region us-east-1 --cluster-id eks-auto-01
```

## Reusing a `--cluster-id` after delete

`kubespin delete` marks a cluster's registry record `decommissioned`
rather than removing it, and `apply` refuses to reuse a cluster ID that is
`decommissioning` or `decommissioned` ("reviving one is not a phase
transition; it is a new cluster"). This is not specific to Autopilot/Auto
Mode, but it is easy to hit while iterating on a throwaway Autopilot cluster
with the same `--cluster-id`:

```text
kubespin: applying gke-autopilot-01: cluster is decommissioning or decommissioned: gke-autopilot-01 is at phase decommissioned
```

There is currently no CLI command to purge a decommissioned record. Options:

- **Pick a different `--cluster-id`** (e.g. append `-02`) — the simplest fix
  for repeated test runs.
- **Delete the registry rows directly**, if you have `KUBESPIN_REGISTRY_DSN`
  access and specifically want the same ID back:
  ```sql
  DELETE FROM cluster_argocd_details WHERE cluster_id = 'gke-autopilot-01';
  DELETE FROM fleet_registry WHERE cluster_id = 'gke-autopilot-01';
  ```

## Azure

`--autopilot` is not available for `--provider azure`. AKS's equivalent
("Automatic" mode or Node Autoprovisioning) requires a beta version of the
Azure SDK that kubespin does not yet depend on; standard AKS node pools with
the `cluster-autoscaler` addon remain the only supported path on Azure for
now.
