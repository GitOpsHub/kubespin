## kubespin fleet bootstrap

Provision the shared fleet infrastructure in the fleet account

### Synopsis

bootstrap creates the Fleet Registry table and the Central Ingestion API,
converging live infrastructure toward the desired state.

It is safe to re-run: every resource is create-or-update, and a run against
already-provisioned infrastructure reports no changes. Nothing is ever deleted.

This provisions shared platform infrastructure and must be run against a
dedicated fleet account that hosts no clusters. The caller's real account is
checked against --account-id before anything is created.

```
kubespin fleet bootstrap [flags]
```

### Options

```
      --account-id string          AWS account ID hosting fleet infrastructure (required)
  -h, --help                       help for bootstrap
      --lambda-binary string       compiled ingestion handler to deploy (default "bin/ingestion/bootstrap")
      --log-retention-days int32   CloudWatch log retention (default 30)
      --name-prefix string         prefix for every provisioned resource name (default "kubespin")
      --throttle-burst int32       ingestion API burst limit (default 100)
      --throttle-rate float        ingestion API steady-state request rate (default 50)
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

