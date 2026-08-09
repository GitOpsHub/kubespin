# kubespin

Provision and manage Kubernetes clusters across EKS, GKE, and AKS, in both
private and public access modes.

Each cluster gets its own Git repository and its own **local** Argo CD instance
that syncs from it. There is no central Argo CD hub and no inbound network
access to any cluster: status flows outward, pushed by an in-cluster reporter to
a central Fleet Registry.

> **Status: early development.** Milestone M0 (foundations) is in place — the
> CLI skeleton, shared domain types, CI, and the fleet infrastructure stack.
> Every command is currently a stub that exits non-zero. See
> [EXECUTION-PLAN.md](EXECUTION-PLAN.md) for what lands next.

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
| `kubespin fleet bootstrap` | Provision the shared fleet infrastructure. Converges, never deletes. |
| `kubespin apply` | Create or reconcile a cluster to match its desired state. Idempotent and resumable. |
| `kubespin delete` | Decommission a cluster; archives its repository rather than deleting it. |
| `kubespin fleet update` | Roll a component version across every matching cluster, in waves. |
| `kubespin fleet audit` | Diff live cloud infrastructure against each cluster's `cluster.yaml`. |
| `kubespin fleet status` | Report sync, drift, and staleness across the fleet. |

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
cmd/kubespin/        binary entrypoint
cmd/ingestion/       Central Ingestion API handler, deployed to Lambda
internal/cli/        cobra command tree and configuration resolution
internal/core/       shared domain types; dependency-free by design
internal/fleetinfra/ SDK-driven converge engine for the fleet infrastructure
internal/version/    build metadata stamped in via -ldflags
```

## Documentation

Start at [docs/](docs/README.md).

- [Architecture](docs/architecture.md) — the outbound-only model, the phase state machine, and why convergence replaces state
- [Fleet bootstrap](docs/fleet-bootstrap.md) — operator runbook, including the IAM permissions needed to run it
- [Development](docs/development.md) — toolchain, testing, and how to add a converge step
- [CLI reference](docs/cli/kubespin.md) — generated from the command tree by `make docs`

Planning documents:

- [IMPLEMENTATION-PLAN-multicloud-k8s-platform-cli.md](IMPLEMENTATION-PLAN-multicloud-k8s-platform-cli.md) — milestones and acceptance criteria
- [EXECUTION-PLAN.md](EXECUTION-PLAN.md) — locked decisions and PR-sized breakdown
