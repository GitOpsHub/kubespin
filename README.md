# kubespin

Provision and manage Kubernetes clusters across EKS, GKE, and AKS, in both
private and public access modes.

Each cluster gets its own Git repository and its own **local** Argo CD instance
that syncs from it. There is no central Argo CD hub, no always-on kubespin
service, and nothing kubespin-owned running inside a cluster: every operation
is a direct connection from the machine running the CLI.

> **Status: every command is implemented and unit-tested against fakes** (cloud
> SDK fakes, an in-memory registry, an in-memory GitHub-shaped repo). None of
> it has run against a real cloud account, GitHub org, or cluster yet — see
> [docs/README.md](docs/README.md#project-status) for details on current testing
> and implementation coverage.

## Quick start

```bash
make bootstrap
```

```bash
make
```

That runs lint, tests, and builds `bin/kubespin` — then installs it to
`~/.local/bin` so every command below works as plain `kubespin`, from any
directory. Override the destination with `make build INSTALL_DIR=...`.

## Commands

| Command | Purpose |
|---|---|
| `kubespin login` / `status` / `logout` | Authenticate to (or check, or clear) cloud provider sessions. |
| `kubespin apply` | Create or reconcile a cluster to match its desired state. Idempotent and resumable. |
| `kubespin delete` | Decommission a cluster; archives its repository rather than deleting it. |

See [Example workflows](#example-workflows) below for real invocations of
each, or [docs/examples.md](docs/examples.md) for the full walkthrough.

## Example workflows

Full detail, prerequisites, and more scenarios in
[docs/examples.md](docs/examples.md). Every command is run from a repository
checkout after `make build`. All three clouds follow the same shape:
authenticate, then `apply`.

The registry DSN has no usable default, so it is never a flag —
`apply`/`delete` read it only from `KUBESPIN_REGISTRY_DSN` (or a `.env`
file). `--size` recurs instead — it defaults to `small`, but every example
below sets it explicitly to show where a bigger cluster changes.

```bash
# AWS, private cluster, default size (small)
kubespin login --only aws
kubespin apply --provider aws --region us-east-1 --cluster-id demo-aws \
  --access private \
  --github-org "$GITHUB_ORG"
```

```bash
# GCP, public cluster, custom node pool
kubespin apply --provider gcp --gcp-project kubernetes-dev-502710 --region us-central1 \
  --cluster-id demo-gcp --access public --size small \
  --instance-type e2-standard-4 --desired-size 3 \
  --github-org "$GITHUB_ORG"
```

> A GKE cluster whose `--region` is a region (not a zone) replicates the
> default node pool across 3 zones, and quota is consumed per zone: the
> command above requests 36 vCPU, and — independent of `--instance-type` —
> 900Gi of boot disk, since GKE's default boot disk is a fixed 100Gi
> regardless of machine type. Low-quota projects should size both down, e.g.
> `--instance-type e2-standard-2 --min-size 1 --max-size 3 --desired-size 1
> --disk-size 30` (6 vCPU, 90Gi total), or request a `CPUS_ALL_REGIONS` /
> `SSD_TOTAL_GB` quota increase for the region first. See
> [docs/examples.md](docs/examples.md#gcp-public-cluster-with-a-larger-node-pool).

```bash
# Azure, medium size (adds Velero + Falco onto the default addon set)
kubespin apply --provider azure --azure-subscription "$AZURE_SUBSCRIPTION_ID" \
  --region eastus --cluster-id demo-azure --access private \
  --size medium \
  --github-org "$GITHUB_ORG"
```

```bash
# Tear down
kubespin delete --provider aws --region us-east-1 --cluster-id demo-aws \
  --github-org "$GITHUB_ORG" --yes
```

`--dry-run` is a root persistent flag, but only `apply` acts on it. `delete`
accepts it and proceeds anyway — see
[which commands honour `--dry-run`](docs/examples.md#which-commands-honour---dry-run).

## Configuration

Precedence is **flags > `KUBESPIN_*` environment variables > config file >
defaults**. The config file is `$XDG_CONFIG_HOME/kubespin/config.yaml` or
`./config.yaml` unless `--config` says otherwise.

```yaml
log-level: info
log-format: text
registry-dsn: postgres://user:pass@host:5432/dbname?sslmode=require
```

Note `registry-dsn` is deliberately not settable via flag — only the config
file or `KUBESPIN_REGISTRY_DSN`, so a connection string carrying a password
never appears in shell history or a process listing.

## Layout

```
cmd/kubespin/               binary entrypoint
internal/cli/               cobra command tree and configuration resolution
internal/core/              shared domain types; dependency-free by design
internal/auth/              operator-facing cloud auth behind login/status/logout
internal/registry/          cluster registry client and lease
internal/orchestrator/      per-cluster phase state machine and reverse teardown
internal/provisioner/       cluster and network interfaces, one impl per cloud
internal/repo/              cluster repositories over GitHub
internal/catalog/           size resolution (small/medium/large, builtin) + per-cluster override patches
internal/argocd/            app-of-apps rendering and Argo CD install
internal/version/           build metadata stamped in via -ldflags
```

## Documentation

Start at [docs/](docs/README.md).

- [Architecture](docs/architecture.md) — the no-agent model, the phase state machine, and why convergence replaces state
- [Examples](docs/examples.md) — working commands for every scenario: spinning up a cluster on each cloud and tearing it down
- [Development](docs/development.md) — toolchain, testing, and how to add a converge step
- [CLI reference](docs/cli/kubespin.md) — generated from the command tree by `make docs`
