## kubespin delete

Decommission a cluster and its supporting resources

### Synopsis

delete performs the teardown in reverse order: mark the cluster
decommissioning in the Fleet Registry, clean up identity and OIDC resources,
delete the cluster, archive its repository, and record it decommissioned.

Repositories are archived, never deleted: history is retained.

```
kubespin delete [flags]
```

### Options

```
      --cluster-id string   cluster identifier
  -h, --help                help for delete
      --yes                 skip the interactive confirmation prompt
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

