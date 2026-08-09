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

That runs lint, tests, and builds `bin/kubespin`.

## Commands

| Command | Purpose |
|---|---|
| `./bin/kubespin login` / `status` / `logout` | Authenticate to (or check, or clear) cloud provider sessions. |
| `./bin/kubespin fleet bootstrap` | Provision the shared fleet infrastructure. Converges, never deletes. |
| `./bin/kubespin apply` | Create or reconcile a cluster to match its desired state. Idempotent and resumable. |
| `./bin/kubespin delete` | Decommission a cluster; archives its repository rather than deleting it. |
| `./bin/kubespin fleet update` | Roll a component version across every matching cluster, in waves. |
| `./bin/kubespin fleet audit` | Diff live cloud infrastructure against each cluster's `cluster.yaml`. |
| `./bin/kubespin fleet status` | Report sync, drift, and staleness across the fleet. |

See [Example workflows](#example-workflows) below for real invocations of
each, or [docs/examples.md](docs/examples.md) for the full walkthrough.

## Example workflows

Full detail, prerequisites, and more scenarios in
[docs/examples.md](docs/examples.md). Every command is run from a repository
checkout after `make build`. All three clouds follow the same shape:
authenticate, `apply`, confirm with `fleet status`.

Two flags recur because neither has a usable default: `--registry-region`
(the Fleet Registry has no default region, on purpose) and `--profile`
(`apply`/`delete` validate a full spec, and a profile reference is part of
one). Export `KUBESPIN_REGISTRY_REGION` to drop the first.

```bash
# AWS, private cluster
./bin/kubespin login --only aws
./bin/kubespin apply --provider aws --region us-east-1 --cluster-id demo-aws \
  --access private --profile tier-small@1.0.0 \
  --github-org "$GITHUB_ORG" --registry-region us-east-1
./bin/kubespin fleet status --phase ready --registry-region us-east-1
```

```bash
# GCP, public cluster, custom node pool
./bin/kubespin apply --provider gcp --gcp-project my-gcp-project --region us-central1 \
  --cluster-id demo-gcp --access public --profile tier-small@1.0.0 \
  --instance-type e2-standard-4 --desired-size 3 \
  --github-org "$GITHUB_ORG" --registry-region us-east-1
```

```bash
# Azure, resolving addons from a platform-profiles repo
./bin/kubespin apply --provider azure --azure-subscription "$AZURE_SUBSCRIPTION_ID" \
  --region eastus --cluster-id demo-azure --access private \
  --profile tier-standard@1.0.0 --profiles-repo platform-profiles \
  --github-org "$GITHUB_ORG" --registry-region us-east-1
```

```bash
# Fleet-wide: bootstrap once, then operate across every cluster
make lambda
./bin/kubespin fleet bootstrap --account-id 111122223333 --registry-region us-east-1
./bin/kubespin fleet status --registry-region us-east-1
./bin/kubespin fleet update --component argo-cd --version 2.11.0 \
  --github-org "$GITHUB_ORG" --registry-region us-east-1
./bin/kubespin fleet audit \
  --github-org "$GITHUB_ORG" --registry-region us-east-1
```

```bash
# Tear down
./bin/kubespin delete --provider aws --region us-east-1 --cluster-id demo-aws \
  --profile tier-small@1.0.0 \
  --github-org "$GITHUB_ORG" --registry-region us-east-1 --yes
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
registry-table: kubespin-fleet-registry
registry-region: us-east-1
```

## Fleet bootstrap

Before any cluster can be provisioned, the shared infrastructure has to exist:
the Fleet Registry table and the Central Ingestion API. Both are created by
`kubespin` itself through the AWS SDK — there is no Terraform, no CloudFormation,
and no second toolchain.

Run it against a dedicated fleet account that hosts no clusters. The caller's
real account is checked against `--account-id` before anything is created, so
pointing it at the wrong account fails instead of half-succeeding.

The ingestion handler is read from disk rather than embedded, so build it
first:

```bash
make lambda
```

```bash
./bin/kubespin fleet bootstrap --account-id <id> --registry-region <region> --dry-run
```

Drop `--dry-run` to apply. Re-running is safe and expected: every resource is
create-or-update, so a second run reports everything in sync. Nothing is ever
deleted — tearing down fleet infrastructure is a deliberate manual act, and the
registry table keeps deletion protection on.

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
- [EXECUTION-PLAN.md](EXECUTION-PLAN.md) — locked decisions and PR-sized breakdown
