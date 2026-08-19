# kubespin status

Show authentication state per cloud provider

## Synopsis

status is read-only: it reports whether each provider's session currently
looks valid, without logging in, logging out, or otherwise changing anything.

Use this to debug "why is my provisioner failing" before assuming the bug is
in kubespin rather than an expired session.

```text
kubespin status [flags]
```

## Examples

```bash
  # Every configured provider
  kubespin status

  # Just Azure
  kubespin status --only azure
```

## Options

```text
  -h, --help           help for status
      --only strings   comma-separated providers to check, e.g. aws,gcp (default: all)
```

## Options inherited from parent commands

```text
      --config string       path to config file (default: $XDG_CONFIG_HOME/kubespin/config.yaml)
      --dry-run             resolve and report intended changes without performing them
      --log-format string   log output format: text or json (default "text")
      --log-level string    log verbosity: debug, info, warn, error (default "info")
```

## See also

* [kubespin](kubespin.md) - Provision and manage Kubernetes clusters across EKS, GKE, and AKS

