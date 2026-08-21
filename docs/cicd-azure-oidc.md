# CI/CD: deploying AKS via GitHub Actions OIDC

[.github/workflows/deploy-aks.yml](https://github.com/GitOpsHub/kubespin/blob/main/.github/workflows/deploy-aks.yml) builds
the binary with `make build` and runs
`kubespin apply --provider azure` from a manually dispatched GitHub
Actions run, authenticating to both clouds it touches with short-lived OIDC
tokens — no long-lived cloud secrets stored in GitHub for either:

- **AWS**, because `apply`'s own auth preflight
  (`cloudAuthProviders` in [internal/cli/apply.go](https://github.com/GitOpsHub/kubespin/blob/main/internal/cli/apply.go))
  unconditionally requires an authenticated AWS session before it will run
  anything — even a dry run, and even for a cluster whose `--provider` is
  `azure`. This is no longer about where the Fleet Registry lives: the
  registry is now a Postgres database reachable over the network from
  wherever it's hosted (not necessarily AWS at all — this deployment's
  instance isn't), reached purely via `KUBESPIN_REGISTRY_DSN`, with no
  IAM-mediated access of any kind. The AWS credential this workflow needs is
  therefore minimal: just enough for `sts:GetCallerIdentity` to succeed,
  which requires no IAM permissions beyond the role being assumable at all.
- **Azure**, because that's where the AKS cluster and its workload identity
  get created.

`internal/provisioner/azure` (`ClusterProvisioner`/`IdentityProvisioner`/
`NetworkProvisioner`) is implemented, so a real (non-dry-run) run creates an
actual AKS cluster.

`apply` also always provisions the cluster's GitHub repo
(`internal/repo`) regardless of cloud, and always reaches the Fleet Registry
over `KUBESPIN_REGISTRY_DSN`, so this pipeline needs two credentials beyond
the two OIDC exchanges: a GitHub token with repo-create scope in the target
org, and the registry's Postgres connection string. GitHub Actions' OIDC
federation authenticates to *cloud providers*; it does not cover GitHub's own
REST API or an arbitrary Postgres database, so both of these stay real
stored secrets — see step 3.

`azure/login` is used even though nothing in the workflow calls `az`
directly: the Go SDK's `azidentity.NewDefaultAzureCredential`, which
`kubespin` uses internally, falls back through several credential sources and
picks up the `az` CLI session `azure/login` leaves authenticated. That's what
lets a plain `go build`ed binary use the OIDC-federated identity without the
workflow having to export `AZURE_FEDERATED_TOKEN_FILE` by hand.

## One-time setup

Run these once per environment (they provision IAM/AD resources — outside
what `kubespin` itself manages, and outside anything this repo's CI can do on
its own since it needs your tenant/account admin rights).

### 1. AWS: let GitHub Actions authenticate for the auth preflight

Create (or reuse) the GitHub OIDC provider in whichever AWS account you're
willing to let this workflow authenticate against — it does not need to be
the account hosting anything cluster- or registry-related, since nothing
beyond `sts:GetCallerIdentity` is ever called against it:

```bash
aws iam create-open-id-connect-provider \
  --url https://token.actions.githubusercontent.com \
  --client-id-list sts.amazonaws.com \
  --thumbprint-list 6938fd4d98bab03faadb97b34396831e3780aea1
```

Trust policy for the role, scoped to this repo and this workflow only —
tighten `sub` further (e.g. to `environment:azure-production`) if you want
per-environment roles:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "Federated": "arn:aws:iam::<account>:oidc-provider/token.actions.githubusercontent.com" },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": { "token.actions.githubusercontent.com:aud": "sts.amazonaws.com" },
        "StringLike": { "token.actions.githubusercontent.com:sub": "repo:GitOpsHub/kubespin:environment:azure-production" }
      }
    }
  ]
}
```

No permissions policy is required beyond the trust policy above:
`sts:GetCallerIdentity` — the only AWS call this workflow's `apply` run ever
makes — needs no IAM grant at all, just a principal that can assume the role.
There is deliberately no `dynamodb:*` (or any other data-plane) statement
here, unlike the DynamoDB-backed registry this replaced: Postgres access
happens over `KUBESPIN_REGISTRY_DSN`, not through this role.

### 2. Azure: federated credential for the App Registration

```bash
az ad app create --display-name kubespin-github-actions
az ad sp create --id <appId>

az ad app federated-credential create \
  --id <appId> \
  --parameters '{
    "name": "kubespin-deploy-aks",
    "issuer": "https://token.actions.githubusercontent.com",
    "subject": "repo:GitOpsHub/kubespin:environment:azure-production",
    "audiences": ["api://AzureADTokenExchange"]
  }'
```

Grant the service principal `Contributor` (or a narrower custom role — AKS,
network, and managed identity write) on the target subscription or resource
group. This is also the identity the `IdentityProvisioner` uses to set up the
OIDC issuer + federated credential + managed identity for the cluster's own
workload identity — a separate federation, one layer down, between the AKS
cluster and its in-cluster workloads.

### 3. GitHub: environment, variables, and secrets

Create a GitHub **environment** named `azure-production` (Settings →
Environments) and require reviewer approval on it — this is what makes
`environment: azure-production` in the workflow gate the apply step behind a
manual sign-off, not just a manual trigger.

Add these as environment **variables** (not secrets — none of them are
sensitive; the OIDC token exchanges are what authenticate to the two cloud
providers, and the two secrets below carry everything else that needs to
stay private):

| Variable | Value |
|---|---|
| `AWS_AUTH_ROLE_ARN` | ARN of the role created in step 1 |
| `AZURE_CLIENT_ID` | The App Registration's application (client) ID |
| `AZURE_TENANT_ID` | Your Azure AD tenant ID |
| `AZURE_SUBSCRIPTION_ID` | Subscription that hosts the AKS cluster |
| `INGESTION_ENDPOINT` | Central Ingestion API host, from `fleet bootstrap` output |
| `CLUSTER_REPO_GITHUB_ORG` | Org cluster repositories are created in (`--github-org`) |
| `GITHUB_ENTERPRISE_BASE_URL` | Only if on GitHub Enterprise Server; leave unset for github.com |
| `GITHUB_ENTERPRISE_UPLOAD_URL` | Same, paired with the base URL |

Add these as environment **secrets** — the exception to "no stored
credentials" above, because neither is authenticated by the Azure OIDC
exchange:

| Secret | Value |
|---|---|
| `KUBESPIN_CLUSTER_REPO_TOKEN` | A PAT or GitHub App installation token with repo-create/push scope in `CLUSTER_REPO_GITHUB_ORG`. Not the ambient `GITHUB_TOKEN` — that's scoped only to the `kubespin` repo itself. |
| `KUBESPIN_REGISTRY_DSN` | The Fleet Registry's Postgres connection string, injected as an environment variable in the workflow step so it never appears in a flag or in shell history. |

## Running it

Actions → **Deploy AKS cluster** → Run workflow, fill in `cluster-id`,
`region`, `access`, `profile`, `kubernetes-version`. Leave `dry-run` checked
for the first run against any new `cluster-id` — it reports the phase apply
would resume from without touching either cloud, same as
`kubespin apply --dry-run` locally (see
[reportPlan](https://github.com/GitOpsHub/kubespin/blob/main/internal/cli/apply.go)).

Uncheck `dry-run` once the plan looks right. The `deploy-aks-<cluster-id>`
concurrency group serializes runs per cluster so two dispatches against the
same cluster queue instead of racing the Fleet Registry's lease.
