## kubespin apply

Create or reconcile a cluster to match its desired state

### Synopsis

apply drives the full provisioning state machine: acquire the cluster lease,
create the cluster, bind workload identity, create and seed its repository,
install Argo CD, and mark the cluster ready.

apply is idempotent and resumable. A repeat run with no changes performs no
cloud calls and produces no commits; a failed run resumes from the phase
recorded in the Fleet Registry.

```
kubespin apply [flags]
```

### Options

```
      --access string       API server exposure: private or public (default "private")
      --cluster-id string   cluster identifier (also the repository suffix)
  -h, --help                help for apply
      --profile string      profile reference from platform-profiles, e.g. tier-small@1.0.0
      --provider string     cloud provider: aws, gcp, or azure
      --region string       cloud region
      --spec string         path to a cluster.yaml, as an alternative to the flags above
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

