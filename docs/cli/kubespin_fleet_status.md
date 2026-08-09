## kubespin fleet status

Report sync, drift, and staleness across the fleet

### Synopsis

status reads the Fleet Registry, which is populated by each cluster's
fleet-status-reporter pushing outward. It never connects to a cluster, so a
cluster that is unreachable shows as stale rather than blocking the command.

```
kubespin fleet status [flags]
```

### Examples

```
  # Every cluster, as a table
  ./bin/kubespin fleet status --registry-region us-east-1

  # Only clusters that have missed their reporting window
  ./bin/kubespin fleet status --stale-only --stale-threshold 30m \
    --registry-region us-east-1

  # Machine-readable output, restricted to one phase
  ./bin/kubespin fleet status --phase ready --output json \
    --registry-region us-east-1
```

### Options

```
  -h, --help                       help for status
      --output string              output format: table or json (default "table")
      --phase string               restrict to clusters in one phase
      --provider string            restrict to one cloud provider
      --stale-only                 show only clusters that have missed their reporting window
      --stale-threshold duration   how long a cluster may go without reporting before it is stale (default 10m0s)
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

* [kubespin fleet](kubespin_fleet.md)	 - Operate on the whole fleet rather than a single cluster

