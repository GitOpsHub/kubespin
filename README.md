# kubespin

Provision and manage Kubernetes clusters across EKS, GKE, and AKS, in both
private and public access modes.

Each cluster gets its own Git repository and its own **local** Argo CD instance
that syncs from it. There is no central Argo CD hub and no inbound network
access to any cluster: status flows outward, pushed by an in-cluster reporter to
a central Fleet Registry.

> **Status: every command is implemented and unit-tested against fakes** (cloud
> SDK fakes, an in-memory registry, an in-memory GitHub-shaped repo). None of
> it has run against a real cloud account, GitHub org, or cluster yet — see
> [docs/README.md](docs/README.md#where-the-project-is) for exactly what that
> does and does not mean per milestone.

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
| `kubespin fleet bootstrap` | Provision the shared fleet infrastructure. Converges, never deletes. |
| `kubespin apply` | Create or reconcile a cluster to match its desired state. Idempotent and resumable. |
| `kubespin delete` | Decommission a cluster; archives its repository rather than deleting it. |
| `kubespin fleet update` | Roll a component version across every matching cluster, in waves. |
| `kubespin fleet audit` | Diff live cloud infrastructure against each cluster's `cluster.yaml`. |
| `kubespin fleet status` | Report sync, drift, and staleness across the fleet. |
| `kubespin fleet dashboard` | Render a static HTML snapshot of fleet sync status, drift, and staleness. |

See [Example workflows](#example-workflows) below for real invocations of
each, or [docs/examples.md](docs/examples.md) for the full walkthrough.

## Example workflows

Full detail, prerequisites, and more scenarios in
[docs/examples.md](docs/examples.md). Every command is run from a repository
checkout after `make build`. All three clouds follow the same shape:
authenticate, `apply`, confirm with `fleet status`.

The Fleet Registry DSN has no usable default, so it is never a flag —
`apply`/`delete`/`fleet` read it only from `KUBESPIN_REGISTRY_DSN` (or a
`.env` file). `--profile` recurs instead because `apply`/`delete` validate a
full spec, and a profile reference is part of one.

```bash
# AWS, private cluster
kubespin login --only aws
kubespin apply --provider aws --region us-east-1 --cluster-id demo-aws \
  --access private --profile tier-small@1.0.0 \
  --github-org "$GITHUB_ORG"
kubespin fleet status --phase ready
```

```bash
# GCP, public cluster, custom node pool
kubespin apply --provider gcp --gcp-project kubernetes-dev-502710 --region us-central1 \
  --cluster-id demo-gcp --access public --profile tier-small@1.0.0 \
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
# Azure, resolving addons from a platform-profiles repo
kubespin apply --provider azure --azure-subscription "$AZURE_SUBSCRIPTION_ID" \
  --region eastus --cluster-id demo-azure --access private \
  --profile tier-standard@1.0.0 --profiles-repo platform-profiles \
  --github-org "$GITHUB_ORG"
```

```bash
# Fleet-wide: bootstrap once, then operate across every cluster
make lambda
kubespin fleet bootstrap --account-id 465532803838 --region us-east-1
kubespin fleet status
kubespin fleet update --component argo-cd --version 2.11.0 \
  --github-org "$GITHUB_ORG"
kubespin fleet audit \
  --github-org "$GITHUB_ORG"
```

```bash
# Tear down
kubespin delete --provider aws --region us-east-1 --cluster-id demo-aws \
  --profile tier-small@1.0.0 \
  --github-org "$GITHUB_ORG" --yes
```

`--dry-run` is a root persistent flag, but only `apply` and `fleet bootstrap`
act on it. `delete` and `fleet update` accept it and proceed anyway — see
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

## Fleet bootstrap

Before any cluster can be provisioned, the shared infrastructure has to exist:
the Central Ingestion API. It is created by `kubespin` itself through the AWS
SDK — there is no Terraform, no CloudFormation, and no second toolchain. The
Fleet Registry itself is a Postgres database the operator provisions
separately and points `KUBESPIN_REGISTRY_DSN` at; it self-migrates its own
schema on first connect, so there is nothing for `fleet bootstrap` to
provision for it.

Run it against a dedicated fleet account that hosts no clusters. The caller's
real account is checked against `--account-id` before anything is created, so
pointing it at the wrong account fails instead of half-succeeding.

The ingestion handler is read from disk rather than embedded, so build it
first:

```bash
make lambda
```

```bash
kubespin fleet bootstrap --account-id <id> --region <region> --dry-run
```

Drop `--dry-run` to apply. Re-running is safe and expected: every resource is
create-or-update, so a second run reports everything in sync. Nothing is ever
deleted — tearing down fleet infrastructure is a deliberate manual act, and
Postgres-level protections (e.g. deletion protection on a managed instance)
are the operator's responsibility, not kubespin's.

The command prints the ingestion endpoint, which every cluster's egress
allowlist must permit.

## Layout

```
cmd/kubespin/               binary entrypoint
cmd/ingestion/              Central Ingestion API handler, deployed to Lambda
cmd/fleet-status-reporter/  in-cluster CronJob that pushes signed status outward
internal/cli/               cobra command tree and configuration resolution
internal/core/              shared domain types; dependency-free by design
internal/auth/              operator-facing cloud auth behind login/status/logout
internal/registry/          Fleet Registry client and lease
internal/orchestrator/      per-cluster phase state machine and reverse teardown
internal/provisioner/       cluster/identity/network interfaces, one impl per cloud
internal/repo/              cluster repositories over GitHub
internal/catalog/           profile resolution
internal/argocd/            app-of-apps rendering and Argo CD install
internal/fleet/             fleet-wide audit, update, and status
internal/fleetinfra/        SDK-driven converge engine for the fleet infrastructure
internal/ingestion/         token verification and write path for the ingestion API
internal/reporter/          the status reporter's summary and signed push logic
internal/version/           build metadata stamped in via -ldflags
```

## Documentation

Start at [docs/](docs/README.md).

- [Architecture](docs/architecture.md) — the outbound-only model, the phase state machine, and why convergence replaces state
- [Examples](docs/examples.md) — working commands for every scenario: spinning up a cluster on each cloud, the fleet lifecycle, and tearing down
- [Fleet bootstrap](docs/fleet-bootstrap.md) — operator runbook, including the IAM permissions needed to run it
- [Development](docs/development.md) — toolchain, testing, and how to add a converge step
- [CLI reference](docs/cli/kubespin.md) — generated from the command tree by `make docs`

Planning documents:

- [IMPLEMENTATION-PLAN-multicloud-k8s-platform-cli.md](IMPLEMENTATION-PLAN-multicloud-k8s-platform-cli.md) — milestones and acceptance criteria
