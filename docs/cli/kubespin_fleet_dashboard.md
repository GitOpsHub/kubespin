# kubespin fleet dashboard

Render a static HTML snapshot of fleet sync status, drift, and staleness

## Synopsis

dashboard reads the Fleet Registry — the same data 'fleet status' reads — and
renders it as a single self-contained HTML file: no server, no external
assets, nothing to deploy. Open the file in a browser, or publish it wherever
static files already get served.

Rows are correlated by cluster ID, which is what the Fleet Registry and every
cluster's own repository already share. A per-cluster commit SHA is not
included: the registry does not track one (that lives in each cluster's own
.state.yaml), so showing it here would need a repository read per cluster
this command does not otherwise make.

Like 'fleet status', this never connects to a cluster — everything shown
comes from what fleet-status-reporter and 'fleet audit' have already pushed
into the registry.

```text
kubespin fleet dashboard [flags]
```

## Examples

```bash
  # Snapshot the whole fleet to fleet-dashboard.html
  kubespin fleet dashboard

  # Only AWS clusters, written somewhere else
  kubespin fleet dashboard --provider aws --output /tmp/fleet.html
```

## Options

```text
  -h, --help                       help for dashboard
      --output string              path to write the rendered HTML snapshot to (default "fleet-dashboard.html")
      --phase string               restrict to clusters in one phase
      --provider string            restrict to one cloud provider
      --stale-threshold duration   how long a cluster may go without reporting before it is stale (default 10m0s)
```

## Options inherited from parent commands

```text
      --config string       path to config file (default: $XDG_CONFIG_HOME/kubespin/config.yaml)
      --dry-run             resolve and report intended changes without performing them
      --log-format string   log output format: text or json (default "text")
      --log-level string    log verbosity: debug, info, warn, error (default "info")
```

## See also

* [kubespin fleet](kubespin_fleet.md) - Operate on the whole fleet rather than a single cluster

