## kubespin logout

Clear cached sessions for one or more cloud providers

```
kubespin logout [flags]
```

### Options

```
  -h, --help           help for logout
      --only strings   comma-separated providers to log out of, e.g. aws,gcp (default: all)
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

