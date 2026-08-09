# CI/CD: deploying AKS via GitHub Actions OIDC

[.github/workflows/deploy-aks.yml](../.github/workflows/deploy-aks.yml) runs
`kubespin apply --provider azure` from a manually dispatched GitHub Actions
run, authenticating to both clouds it touches with short-lived OIDC tokens —
no long-lived cloud secrets stored in GitHub at all:

- **AWS**, because the Fleet Registry (DynamoDB) is always AWS-hosted,
  regardless of which cloud the cluster itself runs on.
- **Azure**, because that's where the AKS cluster and its workload identity
  get created.

`internal/provisioner/azure` (`ClusterProvisioner`/`IdentityProvisioner`/
`NetworkProvisioner`) is implemented, so a real (non-dry-run) run creates an
actual AKS cluster.

`kubespin apply` also always provisions the cluster's GitHub repo
(`internal/repo`) regardless of cloud, so this pipeline needs a **third**
credential beyond the two OIDC exchanges: a GitHub token with repo-create
scope in the target org. GitHub Actions' OIDC federation authenticates to
*cloud providers*; it does not cover GitHub's own REST API, so this one stays
a real stored secret — see step 3.

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

### 1. AWS: let GitHub Actions assume a Fleet Registry role

Create (or reuse) the GitHub OIDC provider in the AWS account that hosts the
Fleet Registry:

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

Permissions policy: the same `dynamodb:GetItem`/`PutItem`/`UpdateItem`/
`Query` shape the orchestrator's registry client uses, scoped to the
`kubespin-fleet-registry` table — narrower than the `fleet bootstrap`
policy in [fleet-bootstrap.md](fleet-bootstrap.md), since `apply` never
creates or reconfigures the table itself.

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

### 3. GitHub: environment, variables, and the repo token

Create a GitHub **environment** named `azure-production` (Settings →
Environments) and require reviewer approval on it — this is what makes
`environment: azure-production` in the workflow gate the apply step behind a
manual sign-off, not just a manual trigger.

Add these as environment **variables** (not secrets — none of them are
sensitive; the OIDC token exchange is what actually authenticates the two
cloud providers):

| Variable | Value |
|---|---|
| `AWS_FLEET_REGISTRY_ROLE_ARN` | ARN of the role created in step 1 |
| `AWS_FLEET_REGISTRY_REGION` | Region hosting the Fleet Registry table |
| `AZURE_CLIENT_ID` | The App Registration's application (client) ID |
| `AZURE_TENANT_ID` | Your Azure AD tenant ID |
| `AZURE_SUBSCRIPTION_ID` | Subscription that hosts the AKS cluster |
| `INGESTION_ENDPOINT` | Central Ingestion API host, from `fleet bootstrap` output |
| `CLUSTER_REPO_GITHUB_ORG` | Org cluster repositories are created in (`--github-org`) |
| `GITHUB_ENTERPRISE_BASE_URL` | Only if on GitHub Enterprise Server; leave unset for github.com |
| `GITHUB_ENTERPRISE_UPLOAD_URL` | Same, paired with the base URL |
| `PROFILES_REPO` | Only if resolving profiles from a `platform-profiles` repo rather than the builtin catalog |

Add one environment **secret** — this is the exception to "no stored
credentials" above, because it authenticates to GitHub's REST API rather than
a cloud provider:

| Secret | Value |
|---|---|
| `KUBESPIN_CLUSTER_REPO_TOKEN` | A PAT or GitHub App installation token with repo-create/push scope in `CLUSTER_REPO_GITHUB_ORG`. Not the ambient `GITHUB_TOKEN` — that's scoped only to the `kubespin` repo itself. |

## Running it

Actions → **Deploy AKS cluster** → Run workflow, fill in `cluster-id`,
`region`, `access`, `profile`, `kubernetes-version`. Leave `dry-run` checked
for the first run against any new `cluster-id` — it reports the phase apply
would resume from without touching either cloud, same as
`kubespin apply --dry-run` locally (see [reportPlan](../internal/cli/apply.go)).

Uncheck `dry-run` once the plan looks right. The `deploy-aks-<cluster-id>`
concurrency group serializes runs per cluster so two dispatches against the
same cluster queue instead of racing the DynamoDB lease.
