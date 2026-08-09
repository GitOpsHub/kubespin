## kubespin

Provision and manage Kubernetes clusters across EKS, GKE, and AKS

### Synopsis

kubespin provisions Kubernetes clusters across AWS, GCP, and Azure, each with
its own repository and its own local Argo CD instance syncing from it.

Clusters are never reached inbound: status flows outward from an in-cluster
reporter to the Fleet Registry.

### Examples

```
  # Spin up the shared fleet infrastructure once, then a cluster
  kubespin login
  kubespin fleet bootstrap --account-id 111122223333 --registry-region us-east-1
  kubespin apply --provider aws --region us-east-1 --cluster-id demo-aws \
    --access private --github-org GitOpsHub
  kubespin fleet status

See "kubespin <command> --help" for flags and more examples on any command.
```

### Options

```
      --config string            path to config file (default: $XDG_CONFIG_HOME/kubespin/config.yaml)
      --dry-run                  resolve and report intended changes without performing them
  -h, --help                     help for kubespin
      --log-format string        log output format: text or json (default "text")
      --log-level string         log verbosity: debug, info, warn, error (default "info")
      --registry-region string   AWS region hosting the Fleet Registry
      --registry-table string    DynamoDB table backing the Fleet Registry (default "kubespin-fleet-registry")
```

### SEE ALSO

* [kubespin apply](kubespin_apply.md)	 - Create or reconcile a cluster to match its desired state
* [kubespin delete](kubespin_delete.md)	 - Decommission a cluster and its supporting resources
* [kubespin fleet](kubespin_fleet.md)	 - Operate on the whole fleet rather than a single cluster
* [kubespin login](kubespin_login.md)	 - Authenticate to every configured cloud provider
* [kubespin logout](kubespin_logout.md)	 - Clear cached sessions for one or more cloud providers
* [kubespin status](kubespin_status.md)	 - Show authentication state per cloud provider

