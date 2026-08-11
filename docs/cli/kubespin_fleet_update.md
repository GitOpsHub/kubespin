# kubespin fleet update

Roll a component version across every matching cluster

## Synopsis

update patches the repository of every cluster matching the given profile,
staged through a rate-limited worker pool.

With --canary-count set, the first N matching clusters (ordered
deterministically by cluster ID, so a wave is reproducible run to run) are
updated first, as a canary wave. If any canary cluster's update fails, the
rest of the fleet is left untouched and reported "skipped" — canarying exists
to catch a bad version before it reaches every cluster, so a canary failure
must stop the rollout rather than continue past it. Only a clean canary wave
rolls to the rest, in a second wave. --canary-count 0 (the default) skips
canarying and updates every matching cluster in one wave.

--provider is the only filter that currently narrows a wave.

update does not honour the global --dry-run flag: a run commits to every
matching cluster's repository. A cluster already at the target version
reports "already up to date" and commits nothing, so re-running a partially
failed wave is safe.

```text
kubespin fleet update [flags]
```

## Examples

```bash
  # Roll a new Argo CD version across every cluster, 8 at a time
  kubespin fleet update --component argo-cd --version 2.11.0 --concurrency 8 \
    --github-org GitOpsHub --registry-region us-east-1

  # Canary the first 3 clusters before rolling to the rest of the fleet
  kubespin fleet update --component cert-manager --version 1.15.1 \
    --canary-count 3 --github-org GitOpsHub --registry-region us-east-1

  # Scope the wave to one cloud
  kubespin fleet update --component cert-manager --version 1.15.1 \
    --provider aws --github-org GitOpsHub --registry-region us-east-1
```

## Options

```text
      --canary-count int           update this many clusters first and abort before the rest of the fleet if any fail (0 disables canarying)
      --component string           addon to update
      --concurrency int            maximum concurrent repository updates (default 4)
      --github-base-url string     GitHub Enterprise API base URL (leave empty for github.com)
      --github-org string          GitHub organization cluster repositories live in
      --github-upload-url string   GitHub Enterprise upload URL (leave empty for github.com)
  -h, --help                       help for update
      --profiles-repo string       platform-profiles repository name to resolve profiles from (uses the builtin catalog if empty)
      --provider string            restrict to one cloud provider
      --version string             target version
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

* [kubespin fleet](kubespin_fleet.md) - Operate on the whole fleet rather than a single cluster

