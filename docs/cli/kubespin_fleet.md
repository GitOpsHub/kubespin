# kubespin fleet

Operate on the whole fleet rather than a single cluster

```text
kubespin fleet [flags]
```

## Examples

```bash
  # The typical fleet lifecycle, in order
  kubespin fleet bootstrap --account-id 111122223333 --registry-region us-east-1
  kubespin fleet status --registry-region us-east-1
  kubespin fleet update --component argo-cd --version 2.11.0 \
    --github-org GitOpsHub --registry-region us-east-1
  kubespin fleet audit --github-org GitOpsHub --registry-region us-east-1
```

## Options

```text
  -h, --help   help for fleet
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
* [kubespin fleet audit](kubespin_fleet_audit.md) - Diff live cloud infrastructure against each cluster's desired state
* [kubespin fleet bootstrap](kubespin_fleet_bootstrap.md) - Provision the shared fleet infrastructure in the fleet account
* [kubespin fleet dashboard](kubespin_fleet_dashboard.md) - Render a static HTML snapshot of fleet sync status, drift, and staleness
* [kubespin fleet status](kubespin_fleet_status.md) - Report sync, drift, and staleness across the fleet
* [kubespin fleet update](kubespin_fleet_update.md) - Roll a component version across every matching cluster

