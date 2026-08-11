# kubespin login

Authenticate to every configured cloud provider

## Synopsis

login authenticates to every cloud provider kubespin talks to — AWS, GCP,
and Azure — skipping any provider whose session already looks valid.

Logins run concurrently: each provider may open a browser, and there is no
dependency between them, so waiting for them one at a time would just be a
needless delay.

```text
kubespin login [flags]
```

## Examples

```bash
  # Log in to every configured provider
  kubespin login

  # Only AWS and GCP
  kubespin login --only aws,gcp

  # Re-authenticate even if the session still looks valid
  kubespin login --force
```

## Options

```text
      --force          re-authenticate even if the session already looks valid
  -h, --help           help for login
      --only strings   comma-separated providers to log in to, e.g. aws,gcp (default: all)
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

