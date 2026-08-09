## kubespin fleet audit

Diff live cloud infrastructure against each cluster's desired state

### Synopsis

audit describes live infrastructure through each cloud's SDK, diffs it against
the cluster.yaml in that cluster's repository, and reports findings. It
detects changes made outside kubespin, such as a manually resized node pool.

audit is read-only: it never reconciles or commits. Persisting findings back
into the Fleet Registry is not yet implemented; this prints them.

```
kubespin fleet audit [flags]
```

### Options

```
      --azure-subscription string   Azure subscription hosting any Azure clusters in the fleet
      --concurrency int             maximum concurrent cluster audits (default 4)
      --gcp-project string          GCP project hosting any GCP clusters in the fleet
      --github-base-url string      GitHub Enterprise API base URL (leave empty for github.com)
      --github-org string           GitHub organization cluster repositories live in
      --github-upload-url string    GitHub Enterprise upload URL (leave empty for github.com)
  -h, --help                        help for audit
      --provider string             restrict to one cloud provider
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

