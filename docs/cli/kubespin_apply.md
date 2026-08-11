# kubespin apply

Create or reconcile a cluster to match its desired state

## Synopsis

apply drives the full provisioning state machine: acquire the cluster lease,
create the cluster, bind workload identity, create and seed its repository,
install Argo CD, and mark the cluster ready.

apply is idempotent and resumable. A repeat run with no changes performs no
cloud calls and produces no commits; a failed run resumes from the phase
recorded in the Fleet Registry.

The spec may come from a cluster.yaml — the same file the cluster's repository
holds — or from the flags below, which override the file when given.

Installing Argo CD is not a push from inside the cluster: apply connects
directly to the API server (via the Helm SDK) from whatever machine runs this
command. For --access private, that means the operator's machine needs
network reachability into the cluster's VPC/VNet (VPN, peering, or a bastion)
— without it, apply will get stuck at the "install argocd" step with a DNS or
connection-timeout error. --access public avoids that, but on GCP it is not
enough by itself: GKE enables master-authorized-networks with an empty
allowlist by default, so nothing (not even the operator) can reach the public
endpoint until --authorized-cidrs includes the caller's IP. AWS and Azure
public endpoints are open to 0.0.0.0/0 unless --authorized-cidrs is set.

That step waits for Argo CD to actually be running, not just for its
manifests to be accepted, so it takes a few minutes on a fresh cluster while
images are pulled. An Argo CD that never becomes ready — pods that cannot be
scheduled or cannot pull — fails the step there rather than looking like
addons that silently never sync.

```text
kubespin apply [flags]
```

## Examples

```bash
  # AWS, private API server, default node pool
  kubespin apply --provider aws --region us-east-1 --cluster-id demo-aws \
    --access private --profile tier-small@1.0.0 \
    --github-org GitOpsHub --registry-region us-east-1

  # GCP, public API server, larger node pool — authorized-cidrs is required on GCP
  # for the operator's own machine to reach the endpoint and install Argo CD
  kubespin apply --provider gcp --gcp-project kubernetes-dev-502710 --region us-central1 \
    --cluster-id demo-gcp --access public --authorized-cidrs 203.0.113.4/32 \
    --profile tier-small@1.0.0 \
    --instance-type e2-medium --desired-size 2 \
    --github-org GitOpsHub --registry-region us-east-1

  # Azure, resolving addons from a platform-profiles repo instead of the builtin catalog
  kubespin apply --provider azure --azure-subscription 3df9adbd-ea55-4c92-964c-0252031979de --region eastus \
    --cluster-id demo-azure --access public --profile tier-standard@1.0.0 \
    --instance-type Standard_D2s_v7 --desired-size 2 \
    --profiles-repo platform-profiles \
    --github-org GitOpsHub --registry-region us-east-1

  # Preview what apply would do without touching any cloud
  kubespin apply --spec ./cluster.yaml --registry-region us-east-1 --dry-run
```

## Options

```text
      --access string               API server exposure: private or public (default "private")
      --authorized-cidrs strings    CIDR blocks allowed to reach the API server when --access public (GCP: required to reach the endpoint at all, since GKE enables master-authorized-networks with an empty allowlist by default; AWS/Azure: public endpoints are open to 0.0.0.0/0 unless this is set)
      --azure-subscription string   Azure subscription hosting the cluster (required for --provider azure)
      --cluster-id string           cluster identifier (also the repository suffix)
      --desired-size int32          desired size of the default node pool (default 2)
      --disk-size int32             boot disk size in GB for the default node pool's nodes (0 = cloud default; GKE regional clusters multiply this by the number of zones, so it is worth setting explicitly on quota-constrained projects)
      --gcp-project string          GCP project hosting the cluster (required for --provider gcp)
      --github-base-url string      GitHub Enterprise API base URL (leave empty for github.com)
      --github-org string           GitHub organization cluster repositories are created in
      --github-upload-url string    GitHub Enterprise upload URL (leave empty for github.com)
  -h, --help                        help for apply
      --ingestion-endpoint string   Central Ingestion API host the cluster must be able to reach
      --instance-type string        instance type for the default node pool (defaults to a cloud-appropriate value per --provider when unset: m6i.large on aws, e2-standard-4 on gcp, Standard_D4s_v7 on azure) (default "m6i.large")
      --kubernetes-version string   Kubernetes minor version, e.g. 1.34
      --max-size int32              maximum size of the default node pool (default 5)
      --min-size int32              minimum size of the default node pool (default 1)
      --profile string              profile reference from platform-profiles, e.g. tier-small@1.0.0
      --profiles-repo string        platform-profiles repository name to resolve profiles from (uses the builtin catalog if empty)
      --provider string             cloud provider: aws, gcp, or azure
      --region string               cloud region
      --spec string                 path to a cluster.yaml describing the cluster
      --subnet-cidr string          address prefix for the subnet kubespin creates when --subnets is omitted (Azure default 10.0.1.0/24, GCP default 10.0.0.0/20)
      --subnets strings             existing subnets to place the cluster in
      --vnet-cidr string            address space for the VNet kubespin creates when --subnets is omitted (Azure only, default 10.0.0.0/16)
      --vpc-cidr string             address space for the VPC kubespin creates when --subnets is omitted (AWS only, default 10.0.0.0/16)
```

## Options inherited from parent commands

```text
      --config string            path to config file (default: $XDG_CONFIG_HOME/kubespin/config.yaml)
      --dry-run                  resolve and report intended changes without performing them
      --log-format string        log output format: text or json (default "text")
      --log-level string         log verbosity: debug, info, warn, error (default "info")
      --registry-region string   AWS region hosting the Fleet Registry
      --registry-table string    DynamoDB table backing the Fleet Registry (default "kubespin-fleet-registry")
```

## See also

* [kubespin](kubespin.md) - Provision and manage Kubernetes clusters across EKS, GKE, and AKS

