# Fleet bootstrap runbook

`kubespin fleet bootstrap` provisions the shared infrastructure every cluster
depends on: the Fleet Registry table and the Central Ingestion API. It is run
once per fleet account, and safely re-run any time after.

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
[internal/fleetinfra/clients.go](../internal/fleetinfra/clients.go), so this list
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
      "Sid": "FleetRegistry",
      "Effect": "Allow",
      "Action": [
        "dynamodb:DescribeTable",
        "dynamodb:CreateTable",
        "dynamodb:UpdateTable",
        "dynamodb:DescribeContinuousBackups",
        "dynamodb:UpdateContinuousBackups"
      ],
      "Resource": "arn:aws:dynamodb:*:<account>:table/kubespin-fleet-registry"
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

Adjust the resource ARNs if you pass a non-default `--name-prefix` or
`--registry-table`.

## What it creates

Six resources, converged in dependency order:

| Resource | Detail |
|---|---|
| **Fleet Registry table** | DynamoDB, on-demand billing. Partition key `ClusterID`. `ProviderPhaseIndex` GSI on `Provider`+`Phase`, projecting all. Encryption at rest, point-in-time recovery, and deletion protection all on. |
| **Log groups** | `/aws/lambda/<prefix>-ingestion` and `/aws/apigateway/<prefix>-ingestion`, created explicitly so retention can be set — an implicitly created group retains forever. |
| **Ingestion role** | Lambda execution role with an inline policy scoped to `GetItem`/`UpdateItem` on the registry table and writes to its own log group. No `Scan`, no `Delete`, no other table. |
| **Ingestion function** | `provided.al2023` on arm64, 256 MB, 10 s timeout. Currently a **501 skeleton** — the real handler lands in M6. |
| **Ingestion API** | HTTP API with one route: `POST /v1/clusters/{clusterId}/status`, Lambda proxy integration, auto-deploying `$default` stage with throttling and access logs. |
| **Invoke permission** | Allows `apigateway.amazonaws.com` to invoke the function, scoped to this one API. |

Only key and index attributes are declared on the table. Everything else the
registry client writes — `Version`, `LastReportedAt`, `LeaseHolder`,
`LeaseExpiresAt`, timestamps — is schemaless and lives only in Go.

The API route has no authorizer by design. Callers authenticate with a
cloud-native workload identity token verified inside the handler, and three
clouds mean three issuers, so a single-issuer JWT authorizer could not express
it.

## Running it

Always start with a dry run. It performs no mutating calls at all — the test
suite asserts this, not just the documentation.

```bash
./bin/kubespin fleet bootstrap --account-id <id> --registry-region <region> --dry-run
```

On a clean account, every resource reports `create`. Apply it:

```bash
./bin/kubespin fleet bootstrap --account-id <id> --registry-region <region>
```

Then **run the dry run again**. This is the acceptance check, not a formality:

```bash
./bin/kubespin fleet bootstrap --account-id <id> --registry-region <region> --dry-run
```

Every resource must report `in sync`. Since there is no state file, a clean
second run is the only evidence that convergence works — it is what a Terraform
`plan` returning no changes was buying. If anything still reports `create` or
`update` here, treat it as a bug and file it rather than re-applying.

The command prints the ingestion endpoint on completion. **Every cluster's
egress allowlist must permit that host** — the allowlist rule is provisioned
per-cloud during cluster creation, so record it now.

### Useful flags

Region and table name come from the global `--registry-region` and
`--registry-table`; the rest are specific to bootstrap. Full list in the
[CLI reference](cli/kubespin_fleet_bootstrap.md).

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
operators run routinely is a footgun. Removing this infrastructure means
disabling deletion protection and deleting resources by hand, which is the
correct amount of friction for an irreversible act.

## Troubleshooting

**`caller account does not match the configured fleet account: credentials belong to X, expected Y`**
Your ambient credentials are for a different account than `--account-id`. This is
the guard working. Check `aws sts get-caller-identity`, then fix whichever of the
two is wrong — do not "fix" it by changing `--account-id` to match, unless you
genuinely intend to bootstrap that account.

**`ingestion handler not found at <path>: build it first with make lambda`**
The handler is read from disk. Run `make lambda`, or point `--lambda-binary` at
an existing build.

**`configuration error: --registry-region is required to bootstrap`**
Region has no default on purpose: silently defaulting would create a second,
empty registry in an unintended region, and the fleet would be split across two
tables with no error anywhere.

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

Exit codes: `0` success, `1` failure, `3` a command that is not implemented yet.
