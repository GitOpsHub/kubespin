## kubespin fleet update

Roll a component version across every matching cluster

### Synopsis

update patches the repository of every cluster matching the given profile,
staged in waves through a rate-limited worker pool, canary tier first.

```
kubespin fleet update [flags]
```

### Options

```
      --component string   addon to update
      --concurrency int    maximum concurrent repository updates (default 4)
  -h, --help               help for update
      --profile string     restrict to clusters on this profile
      --version string     target version
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

