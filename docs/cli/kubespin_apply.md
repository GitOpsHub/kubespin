## kubespin apply

Create or reconcile a cluster to match its desired state

### Synopsis

apply drives the full provisioning state machine: acquire the cluster lease,
create the cluster, bind workload identity, create and seed its repository,
install Argo CD, and mark the cluster ready.

apply is idempotent and resumable. A repeat run with no changes performs no
cloud calls and produces no commits; a failed run resumes from the phase
recorded in the Fleet Registry.

The spec may come from a cluster.yaml — the same file the cluster's repository
holds — or from the flags below, which override the file when given.

```
kubespin apply [flags]
```

### Options

```
      --access string               API server exposure: private or public (default "private")
      --azure-subscription string   Azure subscription hosting the cluster (required for --provider azure)
      --cluster-id string           cluster identifier (also the repository suffix)
      --desired-size int32          desired size of the default node pool (default 2)
      --gcp-project string          GCP project hosting the cluster (required for --provider gcp)
      --github-base-url string      GitHub Enterprise API base URL (leave empty for github.com)
      --github-org string           GitHub organization cluster repositories are created in
      --github-upload-url string    GitHub Enterprise upload URL (leave empty for github.com)
  -h, --help                        help for apply
      --ingestion-endpoint string   Central Ingestion API host the cluster must be able to reach
      --instance-type string        instance type for the default node pool (default "m6i.large")
      --kubernetes-version string   Kubernetes minor version, e.g. 1.34
      --max-size int32              maximum size of the default node pool (default 5)
      --min-size int32              minimum size of the default node pool (default 1)
      --profile string              profile reference from platform-profiles, e.g. tier-small@1.0.0
      --profiles-repo string        platform-profiles repository name to resolve profiles from (uses the builtin catalog if empty)
      --provider string             cloud provider: aws, gcp, or azure
      --region string               cloud region
      --spec string                 path to a cluster.yaml describing the cluster
      --subnets strings             existing subnets to place the cluster in
```

### Options inherited from parent commands

```
      --config string            path to config file (default: $XDG_CONFIG_HOME/kubespin/config.yaml)
      --dry-run                  resolve and report intended changes without performing them
      --log-format string        log output format: text or json (default "text")
      --log-level string         log verbosity: debug, info, warn, error (default "info")
      --registry-region string   AWS region hosting the Fleet Registry
      --registry-table string    DynamoDB table backing the Fleet Registry (default "kubespin-fleet-registry")
```

### SEE ALSO

* [kubespin](kubespin.md)	 - Provision and manage Kubernetes clusters across EKS, GKE, and AKS

