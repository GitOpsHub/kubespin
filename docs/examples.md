# Examples

Working commands to copy, paste, and adjust. For flag-by-flag detail see the
[CLI reference](cli/kubespin.md); for *why* the system behaves this way see
[Architecture](architecture.md); for the fleet-bootstrap deep dive
(permissions, what it creates, troubleshooting) see
[Fleet bootstrap](fleet-bootstrap.md).

Every example below assumes a checkout with `bin/kubespin` built
(`make`) or `kubespin` on `PATH`. Substitute `./bin/kubespin` for `kubespin`
if you haven't installed it.

## Prerequisites

kubespin authenticates to clouds through your existing CLI sessions, not
environment variables — run the provider's own login first, or let
`kubespin login` do it for you (see [Auth workflows](#auth-workflows) below):

```bash
aws sso login
az login
gcloud auth application-default login
```

Two things every non-dry-run `apply`/`delete`/`fleet update`/`fleet audit`
needs, from [`.env.example`](../.env.example):

- **`GITHUB_TOKEN`** — a GitHub token with repo-create/push scope, read from
  the environment (never a flag, so it never lands in shell history).
- **`--github-org`** — the org cluster repositories are created in.
  `GITHUB_ORG` in `.env.example` is a reminder to set this, not something
  kubespin reads directly: export it and pass it yourself, e.g.
  `--github-org "$GITHUB_ORG"`.

And for every command that talks to the Fleet Registry: `--registry-region`
(or `KUBESPIN_REGISTRY_REGION`, or `registry-region` in the config file) —
it has no default, on purpose (see
[Fleet bootstrap troubleshooting](fleet-bootstrap.md#troubleshooting)).

## Auth workflows

```bash
# Log in to every configured provider (AWS today; GCP and Azure as they land)
kubespin login

# Only the providers you need right now
kubespin login --only aws,gcp

# Force re-authentication even if the cached session still looks valid
kubespin login --force

# Check session state without changing anything — useful when a provisioner
# fails and you're not sure if it's a bug or an expired session
kubespin status

# Clear a cached session
kubespin logout --only azure
```

## Spin up a single cluster

Each of these is self-contained: authenticate, apply, confirm it landed.

### AWS, private cluster

```bash
kubespin login --only aws

kubespin apply --provider aws --region us-east-1 --cluster-id demo-aws \
  --access private --github-org "$GITHUB_ORG"

kubespin fleet status --phase ready
```

### GCP, public cluster with a larger node pool

```bash
kubespin login --only gcp

kubespin apply --provider gcp --gcp-project my-gcp-project --region us-central1 \
  --cluster-id demo-gcp --access public --instance-type e2-standard-4 \
  --desired-size 3 --min-size 2 --max-size 6 --github-org "$GITHUB_ORG"

kubespin fleet status --phase ready
```

### Azure, resolving addons from a platform-profiles repo

```bash
kubespin login --only azure

kubespin apply --provider azure --azure-subscription "$AZURE_SUBSCRIPTION_ID" \
  --region eastus --cluster-id demo-azure --access private \
  --profile tier-standard@1.0.0 --profiles-repo platform-profiles \
  --github-org "$GITHUB_ORG"

kubespin fleet status --phase ready
```

`--profile` with no `--profiles-repo` resolves against the builtin catalog —
useful before `platform-profiles` exists yet.

### From a cluster.yaml instead of flags

Flags override the file when both are given, so this also works for a spec
checked out from a cluster's own repository:

```bash
kubespin apply --spec ./cluster.yaml
```

### Preview before applying

Every mutating command accepts `--dry-run` (a root persistent flag). A dry
run for `apply` only reads the Fleet Registry — it never touches the
cluster's own cloud:

```bash
kubespin apply --provider aws --region us-east-1 --cluster-id demo-aws --dry-run
```

## Fleet lifecycle

The shared fleet infrastructure (Fleet Registry + Central Ingestion API) is
provisioned once per fleet account, before any cluster. Full walkthrough,
including required IAM permissions, in [Fleet bootstrap](fleet-bootstrap.md).

```bash
# 1. Preview
kubespin fleet bootstrap --account-id 111122223333 --registry-region us-east-1 --dry-run

# 2. Apply
kubespin fleet bootstrap --account-id 111122223333 --registry-region us-east-1

# 3. Spin up clusters (repeat per cluster; see "Spin up a single cluster" above)
kubespin apply --provider aws --region us-east-1 --cluster-id demo-aws \
  --access private --github-org "$GITHUB_ORG"

# 4. Watch the fleet
kubespin fleet status
kubespin fleet status --stale-only --stale-threshold 30m
kubespin fleet status --output json

# 5. Roll a component version across every matching cluster
kubespin fleet update --component argo-cd --version 2.11.0 --concurrency 8

# Scope a wave to one tier and one cloud
kubespin fleet update --component cert-manager --version 1.15.1 \
  --profile tier-standard@1.0.0 --provider aws

# 6. Check live infra against each cluster's cluster.yaml
kubespin fleet audit
kubespin fleet audit --provider gcp --concurrency 8
```

## Tear down

Repositories are archived, never deleted — see
[Architecture](architecture.md) for why delete is a reverse teardown rather
than the inverse of a Terraform destroy.

```bash
# Interactive: prompts to type the cluster ID to confirm
kubespin delete --provider aws --region us-east-1 --cluster-id demo-aws \
  --github-org "$GITHUB_ORG"

# Scripted: skip the confirmation prompt
kubespin delete --provider gcp --gcp-project my-gcp-project --region us-central1 \
  --cluster-id demo-gcp --github-org "$GITHUB_ORG" --yes

# Using the same cluster.yaml apply was run with
kubespin delete --spec ./cluster.yaml --yes
```

There is no fleet-infrastructure teardown command, deliberately — see
[Fleet bootstrap: re-running, resuming, and tearing down](fleet-bootstrap.md#re-running-resuming-and-tearing-down).

## Dry-run everywhere

`--dry-run` is a root persistent flag, so it composes with any mutating
command: `apply`, `delete`, and `fleet bootstrap` all honor it, reporting
what they would do without touching a cloud, a repository, or the Fleet
Registry's write path.
