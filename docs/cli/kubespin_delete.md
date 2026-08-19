# kubespin delete

Decommission a cluster and its supporting resources

## Synopsis

delete performs the teardown in reverse order: mark the cluster
decommissioning in the Fleet Registry, clean up identity and OIDC resources,
delete the cluster, archive its repository, and record it decommissioned.

Repositories are archived, never deleted: history is retained.

delete is idempotent and resumable exactly like apply: a cluster already
decommissioned is a no-op, and a failed teardown resumes from
decommissioning on retry rather than needing to be reasoned about by hand.

The spec identifies which cluster and cloud to tear down. It may come from a
cluster.yaml — the same file the cluster's repository holds — or from the
flags below, which override the file when given.

delete does not honour the global --dry-run flag: passing it does not make
this command a preview. Use --yes to skip the confirmation prompt only when
you mean it.

```text
kubespin delete [flags]
```

## Examples

```bash
  # AWS, prompts to type the cluster ID to confirm
  kubespin delete --provider aws --region us-east-1 --cluster-id demo-aws \
    --profile tier-small@1.0.0 --github-org GitOpsHub

  # GCP, scripted (no interactive confirmation)
  kubespin delete --provider gcp --gcp-project kubernetes-dev-502710 --region us-central1 \
    --cluster-id demo-gcp --profile tier-small@1.0.0 \
    --github-org GitOpsHub --yes

  # Using the same cluster.yaml apply was run with
  kubespin delete --spec ./cluster.yaml \
    --github-org GitOpsHub --yes
```

## Options

```text
      --access string               API server exposure: private or public (must match the cluster's spec) (default "private")
      --authorized-cidrs strings    unused by delete, kept for spec compatibility
      --azure-subscription string   Azure subscription hosting the cluster (required for --provider azure)
      --cluster-id string           cluster identifier
      --desired-size int32          desired size of the default node pool (unused by delete, kept for spec compatibility) (default 2)
      --disk-size int32             boot disk size in GB for the default node pool's nodes (unused by delete, kept for spec compatibility)
      --gcp-project string          GCP project hosting the cluster (required for --provider gcp)
      --gcp-public-nodes            unused by delete, kept for spec compatibility
      --github-base-url string      GitHub Enterprise API base URL (leave empty for github.com)
      --github-org string           GitHub organization the cluster repository lives in
      --github-upload-url string    GitHub Enterprise upload URL (leave empty for github.com)
  -h, --help                        help for delete
      --instance-type string        instance type for the default node pool (unused by delete, kept for spec compatibility) (default "m6i.large")
      --kubernetes-version string   Kubernetes minor version, e.g. 1.34 (unused by delete, kept for spec compatibility)
      --max-size int32              maximum size of the default node pool (unused by delete, kept for spec compatibility) (default 5)
      --min-size int32              minimum size of the default node pool (unused by delete, kept for spec compatibility) (default 1)
      --profile string              profile reference from platform-profiles, e.g. tier-small@1.0.0
      --provider string             cloud provider: aws, gcp, or azure
      --region string               cloud region
      --spec string                 path to a cluster.yaml describing the cluster
      --spot                        unused by delete, kept for spec compatibility
      --subnet-cidr string          unused by delete, kept for spec compatibility
      --subnets strings             existing subnets the cluster was placed in
      --vnet-cidr string            unused by delete, kept for spec compatibility
      --vpc-cidr string             unused by delete, kept for spec compatibility
      --yes                         skip the interactive confirmation prompt
      --zone string                 unused by delete, kept for spec compatibility
```

## Options inherited from parent commands

```text
      --config string       path to config file (default: $XDG_CONFIG_HOME/kubespin/config.yaml)
      --dry-run             resolve and report intended changes without performing them
      --log-format string   log output format: text or json (default "text")
      --log-level string    log verbosity: debug, info, warn, error (default "info")
```

## See also

* [kubespin](kubespin.md) - Provision and manage Kubernetes clusters across EKS, GKE, and AKS

