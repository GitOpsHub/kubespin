## kubespin fleet update

Roll a component version across every matching cluster

### Synopsis

update patches the repository of every cluster matching the given profile,
staged through a rate-limited worker pool.

Canary-first staging (updating a canary tier before the rest of the fleet)
is not yet implemented: every matching cluster is updated in the same wave.
Use --profile to scope a wave to one tier by hand in the meantime.

```
kubespin fleet update [flags]
```

### Examples

```
  # Roll a new Argo CD version across every cluster, 8 at a time
  kubespin fleet update --component argo-cd --version 2.11.0 --concurrency 8

  # Scope the wave to one tier and one cloud
  kubespin fleet update --component cert-manager --version 1.15.1 \
    --profile tier-standard@1.0.0 --provider aws
```

### Options

```
      --component string           addon to update
      --concurrency int            maximum concurrent repository updates (default 4)
      --github-base-url string     GitHub Enterprise API base URL (leave empty for github.com)
      --github-org string          GitHub organization cluster repositories live in
      --github-upload-url string   GitHub Enterprise upload URL (leave empty for github.com)
  -h, --help                       help for update
      --profile string             restrict to clusters on this profile
      --profiles-repo string       platform-profiles repository name to resolve profiles from (uses the builtin catalog if empty)
      --provider string            restrict to one cloud provider
      --version string             target version
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

