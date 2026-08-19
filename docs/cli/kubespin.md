# kubespin

Provision and manage Kubernetes clusters across EKS, GKE, and AKS

## Synopsis

kubespin provisions Kubernetes clusters across AWS, GCP, and Azure, each with
its own repository and its own local Argo CD instance syncing from it.

Clusters are never reached inbound: status flows outward from an in-cluster
reporter to the Fleet Registry.

## Examples

```bash
  # Spin up the shared fleet infrastructure once, then a cluster.
  # KUBESPIN_REGISTRY_DSN must be set (in .env or the environment) throughout.
  kubespin login
  make lambda
  kubespin fleet bootstrap --account-id 465532803838 --region us-east-1
  kubespin apply --provider aws --region us-east-1 --cluster-id demo-aws \
    --access private --profile tier-small@1.0.0 \
    --github-org GitOpsHub
  kubespin fleet status

See "kubespin <command> --help" for flags and more examples on any command.
```

## Options

```text
      --config string       path to config file (default: $XDG_CONFIG_HOME/kubespin/config.yaml)
      --dry-run             resolve and report intended changes without performing them
  -h, --help                help for kubespin
      --log-format string   log output format: text or json (default "text")
      --log-level string    log verbosity: debug, info, warn, error (default "info")
```

## See also

* [kubespin apply](kubespin_apply.md) - Create or reconcile a cluster to match its desired state
* [kubespin delete](kubespin_delete.md) - Decommission a cluster and its supporting resources
* [kubespin fleet](kubespin_fleet.md) - Operate on the whole fleet rather than a single cluster
* [kubespin login](kubespin_login.md) - Authenticate to every configured cloud provider
* [kubespin logout](kubespin_logout.md) - Clear cached sessions for one or more cloud providers
* [kubespin status](kubespin_status.md) - Show authentication state per cloud provider

