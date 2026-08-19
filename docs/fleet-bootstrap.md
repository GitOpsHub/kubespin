# Fleet bootstrap runbook

`kubespin fleet bootstrap` provisions the shared infrastructure every cluster
depends on: the Central Ingestion API. It is run once per fleet account, and
safely re-run any time after.

The Fleet Registry itself is a Postgres database, provisioned and operated
separately from this command (it can be hosted anywhere reachable over the
network, not necessarily in this AWS account). `kubespin` connects to it via
`KUBESPIN_REGISTRY_DSN` and self-migrates its schema on first connect —
there is nothing for `fleet bootstrap` to create for it.

There is no Terraform and no CloudFormation. The command talks to AWS directly,
converging live infrastructure toward the desired state. See
[Architecture](architecture.md#convergence-without-a-state-file) for what that
trades away and what replaces it.

## Before you start

You need three things:

1. **A dedicated AWS account** that hosts no clusters. This stack is shared
   platform infrastructure; keeping it out of any cluster account limits the
   blast radius of a compromised cluster. The command enforces this — it checks
   the caller's real account against `--account-id` and aborts on a mismatch.
2. **Credentials** for that account in the ambient chain (profile, environment,
   SSO, or an assumed role), with the permissions below.
3. **The compiled ingestion handler**, which is read from disk rather than
   embedded in the binary:

```bash
make lambda
```

That produces `bin/ingestion/bootstrap` — a static Linux arm64 executable, which
is what the `provided.al2023` runtime expects. Because the handler is read from
disk, bootstrapping needs a repository checkout, not just the `kubespin` binary.

## Permissions the operator needs

Derived from the calls the converge engine actually makes — every one is
declared in the narrow interfaces in
[internal/fleetinfra/clients.go](https://github.com/GitOpsHub/kubespin/blob/main/internal/fleetinfra/clients.go), so this list
and the code cannot silently diverge.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "VerifyAccount",
      "Effect": "Allow",
      "Action": "sts:GetCallerIdentity",
      "Resource": "*"
    },
    {
      "Sid": "LogGroups",
      "Effect": "Allow",
      "Action": [
        "logs:DescribeLogGroups",
        "logs:CreateLogGroup",
        "logs:PutRetentionPolicy"
      ],
      "Resource": "arn:aws:logs:*:<account>:log-group:/aws/*"
    },
    {
      "Sid": "IngestionRole",
      "Effect": "Allow",
      "Action": [
        "iam:GetRole",
        "iam:CreateRole",
        "iam:GetRolePolicy",
        "iam:PutRolePolicy",
        "iam:PassRole"
      ],
      "Resource": "arn:aws:iam::<account>:role/kubespin-ingestion"
    },
    {
      "Sid": "IngestionFunction",
      "Effect": "Allow",
      "Action": [
        "lambda:GetFunction",
        "lambda:CreateFunction",
        "lambda:UpdateFunctionCode",
        "lambda:UpdateFunctionConfiguration",
        "lambda:GetPolicy",
        "lambda:AddPermission"
      ],
      "Resource": "arn:aws:lambda:*:<account>:function:kubespin-ingestion"
    },
    {
      "Sid": "IngestionApi",
      "Effect": "Allow",
      "Action": ["apigateway:GET", "apigateway:POST", "apigateway:PATCH"],
      "Resource": "arn:aws:apigateway:*::/apis*"
    }
  ]
}
```

Note there is no `dynamodb:*` (or any other database-service) statement here:
Postgres reachability is a VPC/security-group/network concern for the operator
who runs the database, not something the AWS IAM policy running `kubespin` has
any say over.

Three things this list is easy to get wrong:

- **`iam:PassRole` is required** even though nothing in the code calls it.
  Creating the Lambda passes the execution role to the Lambda service, and that
  is a distinct permission from creating the role.
- **API Gateway v2 does not use per-call action names.** The SDK calls map onto
  `apigateway:GET` / `POST` / `PATCH` against `/apis*`, not
  `apigatewayv2:CreateApi`.
- **Access logging needs more than `logs:CreateLogGroup`.** Attaching access log
  settings to an HTTP API stage requires the log-delivery permissions
  (`logs:CreateLogDelivery`, `logs:GetLogDelivery`, `logs:UpdateLogDelivery`,
  `logs:ListLogDeliveries`, `logs:PutResourcePolicy`,
  `logs:DescribeResourcePolicies`). If stage creation fails with an access-denied
  error mentioning log delivery, this is why.

Adjust the resource ARNs if you pass a non-default `--name-prefix`.

## What it creates

Five resources, converged in dependency order:

| Resource | Detail |
|---|---|
| **Log groups** | `/aws/lambda/<prefix>-ingestion` and `/aws/apigateway/<prefix>-ingestion`, created explicitly so retention can be set — an implicitly created group retains forever. |
| **Ingestion role** | Lambda execution role with an inline policy scoped only to `logs:CreateLogStream`/`logs:PutLogEvents` on its own log group. No registry-access statement at all — the function reaches Postgres over the network via `REGISTRY_DSN`, not through an IAM-mediated AWS API. |
| **Ingestion function** | `provided.al2023` on arm64, 256 MB, 10 s timeout. Runs the real handler ([cmd/ingestion](https://github.com/GitOpsHub/kubespin/tree/main/cmd/ingestion)): it verifies the caller's workload identity token against the OIDC issuer recorded for that `{clusterId}`, then writes the push to the registry over `REGISTRY_DSN`. |
| **Ingestion API** | HTTP API with one route: `POST /v1/clusters/{clusterId}/status`, Lambda proxy integration, auto-deploying `$default` stage with throttling and access logs. |
| **Invoke permission** | Allows `apigateway.amazonaws.com` to invoke the function, scoped to this one API. |

The Postgres schema itself (the `fleet_registry` table and its
`(provider, phase)` index) is created idempotently by `registry.NewPostgres`
the first time any `kubespin` command connects — bootstrap never touches it.

The API route has no authorizer by design. Callers authenticate with a
cloud-native workload identity token verified inside the handler, and three
clouds mean three issuers, so a single-issuer JWT authorizer could not express
it.

## Running it

`KUBESPIN_REGISTRY_DSN` must be set (env or `.env`) before running this
command — bootstrap fails fast with a clear error otherwise, since without a
reachable registry nothing downstream can proceed either.

Always start with a dry run. It performs no mutating calls at all — the test
suite asserts this, not just the documentation.

```bash
kubespin fleet bootstrap --account-id <id> --region <region> --dry-run
```

On a clean account, every resource reports `create`. Apply it:

```bash
kubespin fleet bootstrap --account-id <id> --region <region>
```

Then **run the dry run again**. This is the acceptance check, not a formality:

```bash
kubespin fleet bootstrap --account-id <id> --region <region> --dry-run
```

Every resource must report `in sync`. Since there is no state file, a clean
second run is the only evidence that convergence works — it is what a Terraform
`plan` returning no changes was buying. If anything still reports `create` or
`update` here, treat it as a bug and file it rather than re-applying.

The command prints the ingestion endpoint on completion. **Every cluster's
egress allowlist must permit that host** — the allowlist rule is provisioned
per-cloud during cluster creation, so record it now.

### Useful flags

`--region` is `fleet bootstrap`'s own required flag — the AWS region hosting
the ingestion Lambda, IAM role, and API Gateway — separate from the Fleet
Registry's Postgres DSN, which comes only from `KUBESPIN_REGISTRY_DSN` (there
is deliberately no flag for it, so a connection string carrying a password
never appears in shell history). The rest of the flags below are specific to
bootstrap. Full list in the [CLI reference](cli/kubespin_fleet_bootstrap.md).

| Flag | Why you would change it |
|---|---|
| `--name-prefix` | Running a second, isolated stack in the same account |
| `--log-retention-days` | Compliance retention requirements |
| `--throttle-burst`, `--throttle-rate` | Sizing against fleet size × reports per interval |
| `--lambda-binary` | The handler is somewhere other than `bin/ingestion/bootstrap` |

Configuration follows the usual precedence: flags, then `KUBESPIN_*` environment
variables, then the config file, then defaults.

## Re-running, resuming, and tearing down

**Re-running is expected.** Every step is create-or-update. Run it after
upgrading the handler, after changing retention or throttles, or just to confirm
nothing has drifted.

**A failed run resumes.** Steps execute in dependency order and stop at the first
error, leaving earlier resources created and later ones untouched. Fix the cause
and run again — because every step is create-or-update, the second run adopts
what already exists rather than colliding with it. The report is printed even on
failure, so you can see exactly how far it got.

**There is no teardown path, deliberately.** The Fleet Registry is the single
source of durable fleet state, and a `--destroy` flag on the same command that
operators run routinely is a footgun. Removing the ingestion infrastructure
means deleting resources by hand; removing the registry itself is entirely
outside this command's scope — it means dropping the Postgres database (or
disabling whatever deletion protection the hosting provider offers), which is
the correct amount of friction for an irreversible act.

## Troubleshooting

**`caller account does not match the configured fleet account: credentials belong to X, expected Y`**
Your ambient credentials are for a different account than `--account-id`. This is
the guard working. Check `aws sts get-caller-identity`, then fix whichever of the
two is wrong — do not "fix" it by changing `--account-id` to match, unless you
genuinely intend to bootstrap that account.

**`ingestion handler not found at <path>: build it first with make lambda`**
The handler is read from disk. Run `make lambda`, or point `--lambda-binary` at
an existing build.

**`configuration error: the Postgres registry DSN is required (KUBESPIN_REGISTRY_DSN)`**
The DSN has no default and no flag, on purpose: silently defaulting or
accepting it as a flag risks a fleet split across two databases, or a password
leaking into shell history. Set `KUBESPIN_REGISTRY_DSN` (directly or via
`.env`) before running.

**`configuration error: reading --region: ...` / missing `--region`**
`fleet bootstrap` requires its own `--region` (the AWS region for the Lambda,
IAM role, and API Gateway), separate from the registry DSN. It has no default
on purpose: silently defaulting risks provisioning ingestion infrastructure in
an unintended region.

**`invalid fleet infrastructure spec: account id "..." must be 12 digits`**
Validation runs before any AWS call. Note that all spec problems are reported
together, so fix everything listed rather than one per run.

**`applying <step>: ...` with an access-denied error**
Compare the failing call against the policy above. Access-denied on stage
creation is usually the log-delivery permissions; access-denied on
`CreateFunction` is usually the missing `iam:PassRole`.

**A resource keeps reporting `update` on every run**
Convergence is broken for that step — its `Plan` is detecting a difference its
`Apply` does not fix. This is a bug. The `Details` on the report line name the
specific field.

Exit codes: `0` success, `1` failure. Failures print to stderr prefixed with
`kubespin:`; the converge report is printed first, even on failure, so a
partial run always shows how far it got.
