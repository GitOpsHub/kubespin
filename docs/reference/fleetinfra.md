# internal/fleetinfra

`internal/fleetinfra` is the SDK converge engine behind `kubespin fleet
bootstrap`. It provisions the shared, once-per-fleet-account infrastructure
directly through `aws-sdk-go-v2` — no Terraform, no CloudFormation, no state
file. It creates the Fleet Registry (a DynamoDB table with a `ProviderPhaseIndex`
GSI, PITR, SSE, and deletion protection), the ingestion Lambda's execution
IAM role and its CloudWatch log groups, the ingestion Lambda function itself
(`provided.al2023` on arm64), and the Central Ingestion API (an HTTP API on
API Gateway v2 with a Lambda proxy integration, a single
`POST /v1/clusters/{clusterId}/status` route, and an auto-deploying `$default`
stage) plus the Lambda permission that lets API Gateway invoke it.

Because there is no state file, convergence is the contract: every step
describes live AWS state, diffs it against `Spec`, and is create-or-update —
nothing ever deletes, so tearing down fleet infrastructure stays a deliberate
manual act. `Plan` is strictly read-only; it is what `--dry-run` runs, and the
only difference between a dry and a real run is whether `Apply` is then
called. A run against already-provisioned infrastructure must report zero
changes.

## Interfaces

Each AWS service is reached through a narrow interface listing only the calls
the package makes (`internal/fleetinfra/clients.go`). This is what lets the
whole converge engine be unit-tested without credentials, and documents the
exact blast radius of the permissions a bootstrap operator needs.

- **`stsAPI`** — `GetCallerIdentity`. Implemented by `sts.Client` (`github.com/aws/aws-sdk-go-v2/service/sts`).
- **`dynamoAPI`** — `DescribeTable`, `CreateTable`, `UpdateTable`, `DescribeContinuousBackups`, `UpdateContinuousBackups`. Implemented by `dynamodb.Client`.
- **`logsAPI`** — `DescribeLogGroups`, `CreateLogGroup`, `PutRetentionPolicy`. Implemented by `cloudwatchlogs.Client`.
- **`iamAPI`** — `GetRole`, `CreateRole`, `GetRolePolicy`, `PutRolePolicy`. Implemented by `iam.Client`.
- **`lambdaAPI`** — `GetFunction`, `CreateFunction`, `UpdateFunctionCode`, `UpdateFunctionConfiguration`, `GetPolicy`, `AddPermission`. Implemented by `lambda.Client`.
- **`apiGatewayAPI`** — `GetApis`, `CreateApi`, `GetIntegrations`, `CreateIntegration`, `GetRoutes`, `CreateRoute`, `GetStage`, `CreateStage`, `UpdateStage`. Implemented by `apigatewayv2.Client`.

All six are bundled into `*Clients`, built for real use by `NewClients`.

## Exported types

### `Spec`

*(`fleetinfra.go`)* The desired state of the fleet infrastructure, passed to `Converge`.

```go
type Spec struct {
    AccountID string
    Region    string

    NamePrefix       string
    RegistryTable    string
    LogRetentionDays int32
    ThrottleBurst    int32
    ThrottleRate     float64

    LambdaZip []byte
}
```

- `AccountID` — the fleet account; checked against the caller's real STS
  identity before anything is provisioned (`ErrAccountMismatch` on mismatch).
- `LambdaZip` — the packaged ingestion handler produced by `PackageLambda`.
- Unset tunables (`NamePrefix`, `LogRetentionDays`, `ThrottleBurst`,
  `ThrottleRate`) are filled from the `Default*` constants by the unexported
  `withDefaults` method before use.
- `Validate() error` reports every problem at once (via `errors.Join`):
  account id must be 12 digits, `Region` required, `RegistryTable` required,
  `LambdaZip` non-empty. Errors wrap `ErrSpec`.
- Unexported helper methods derive resource names/ARNs from the spec
  (`functionName`, `roleName`, `apiName`, `lambdaLogGroup`, `apiLogGroup`,
  `tableARN`, `roleARN`, `functionARN`, `invokeARN`, `lambdaLogGroupARN`,
  `apiLogGroupARN`). The partition is assumed to be `aws` — GovCloud/China
  would need this threaded through the spec.

### `ActionKind`

*(`fleetinfra.go`)* An enum of what a step's plan intends to do:
`ActionNone`, `ActionCreate`, `ActionUpdate`. There is no `ActionDelete` —
Converge never deletes. `String()` renders `"in sync"`, `"create"`, `"update"`.

### `Action`

*(`fleetinfra.go`)* One step's verdict on one resource.

```go
type Action struct {
    Resource string
    Kind     ActionKind
    Details  []string
}
```

`Details` explains what differs and is printed on both dry and real runs.
`String()` formats as `"<resource>  <kind> (<details>)"`.

### `Option` / `WithLogger`

*(`fleetinfra.go`)* `Option` is a functional option (`func(*options)`) for
configuring a `Converge` run. `WithLogger(logger *slog.Logger) Option` sets
the `*slog.Logger` the run narrates itself through; a nil logger is ignored,
leaving `slog.Default()` in place.

### `Report`

*(`fleetinfra.go`)* The outcome of a converge run.

```go
type Report struct {
    DryRun       bool
    Actions      []Action
    IngestionURL string
}
```

- `IngestionURL` is the full endpoint clusters push status to (base API
  endpoint + `/v1/clusters/{clusterId}/status`), and the host every cluster's
  egress allowlist must permit. Empty on a dry run that would still have to
  create the API.
- `Changed() int` — a method that counts actions whose `Kind != ActionNone`.

### `Clients`

*(`clients.go`)* Bundles the six AWS service interfaces (`sts`, `dynamo`,
`logs`, `iam`, `lambda`, `apiGateway`) that converge steps use. All fields are
unexported; construct via `NewClients`. `(*Clients).verifyAccount(ctx,
want string) error` is the guard that replaces Terraform's
`allowed_account_ids` — it calls `GetCallerIdentity` and refuses to
provision if the caller's account doesn't match `want`, which is what keeps
fleet infrastructure out of a cluster account.

## Errors

```go
var (
    ErrSpec            = errors.New("invalid fleet infrastructure spec")
    ErrAccountMismatch = errors.New("caller account does not match the configured fleet account")
)
```

Both are wrapped (`fmt.Errorf("%w: ...")`) rather than returned bare, so
callers can match with `errors.Is`.

## Exported functions

### `NewClients`

```go
func NewClients(ctx context.Context, region string) (*Clients, error)
```

Loads the ambient AWS credential chain via `config.LoadDefaultConfig` scoped
to `region`, and constructs real `sts`/`dynamodb`/`cloudwatchlogs`/`iam`/
`lambda`/`apigatewayv2` clients from it.

### `PackageLambda`

```go
func PackageLambda(binaryPath string) ([]byte, error)
```

*(`package.go`)* Opens the compiled handler binary at `binaryPath` and returns
a deployable zip (delegates to the unexported `packageBinary`). The zip
contains exactly one entry, named `bootstrap` (the `handlerName` constant —
what `provided.al2023` executes), mode `0o755`, deflated. Every entry's
modified timestamp is pinned to `zipEpoch` (1980-01-01 UTC): determinism is
load-bearing, because convergence in `functionStep.Plan` compares the
archive's SHA-256 against the deployed function's `CodeSha256`, and an
archive carrying the current time would hash differently on every run and
report drift forever.

### `Converge`

```go
func Converge(ctx context.Context, c *Clients, spec Spec, dryRun bool, opts ...Option) (Report, error)
```

*(`fleetinfra.go`)* Brings the fleet infrastructure to match `spec`. Applies
`spec.withDefaults()`, validates it, and verifies the caller's account before
touching anything. Runs six steps in dependency order — registry table, log
groups, IAM role, Lambda function, ingestion API, invoke permission — stopping
at the first error, so a failure leaves earlier resources created and later
ones untouched; re-running resumes, since every step is create-or-update.

For each step: `Plan` runs first (always, dry or real) and its `Action` is
appended to `Report.Actions`. If the action is `ActionNone`, the step is
logged at Debug and skipped. On a dry run, a would-be change is logged at Info
and *not* applied. On a real run, `Apply` is called and any error aborts the
whole `Converge` call. `Report.IngestionURL` is populated from the API step's
resolved endpoint before returning.

## Steps (unexported, one per file)

Each step implements the internal `step` interface (`Name() string`, `Plan(ctx)
(Action, error)`, `Apply(ctx, Action) error`) and is constructed by an
unexported `newXStep` function taking `(*Clients, Spec, *slog.Logger)`.

- **`registryTableStep`** (`step_registry.go`) — provisions the Fleet
  Registry: partition key `ClusterID`, the `ProviderPhaseIndex` GSI
  (`Provider` hash / `Phase` range, projecting all attributes, created with
  the table since adding a GSI later is a slow online backfill), pay-per-request
  billing, SSE, deletion protection, and point-in-time recovery. It only ever
  creates or strengthens the table — turns protections on, adds the missing
  index — never removes anything. Structural changes (`UpdateTable`,
  `UpdateContinuousBackups`) are serialised behind a poll loop (`waitActive`)
  because DynamoDB requires the table to be `ACTIVE` and permits only one
  structural change at a time.
- **`logGroupsStep`** (`step_logs.go`) — provisions both the Lambda's and the
  API's CloudWatch log groups up front, so retention (`LogRetentionDays`) can
  be set at all (an implicitly-created group retains forever) and so the
  Lambda's execution policy can be scoped to a group that already exists.
- **`roleStep`** (`step_iam.go`) — provisions the ingestion Lambda's execution
  IAM role, with a deliberately tiny inline policy (`ingestion`): only
  `dynamodb:GetItem`/`UpdateItem` on the registry table ARN and
  `logs:CreateLogStream`/`PutLogEvents` on its own log group — no
  `CreateTable`, `Scan`, `Delete`, or access to any other table. Policy
  equality is checked semantically (`policyEqual`, via JSON round-trip)
  against a policy IAM may have reformatted.
- **`functionStep`** (`step_lambda.go`) — provisions the ingestion Lambda:
  `provided.al2023` runtime, arm64, 10s timeout, 256MB memory, one env var
  `REGISTRY_TABLE`. Drift on code is detected by comparing the deployed
  `CodeSha256` against `codeSHA256(spec.LambdaZip)` (`package.go`), which is
  why the zip must be byte-deterministic.
- **`apiStep`** (`step_api.go`) — provisions the Central Ingestion API as one
  step covering the HTTP API, its AWS_PROXY Lambda integration, the single
  `POST /v1/clusters/{clusterId}/status` route (`AuthorizationType: NONE` —
  the caller authenticates via a cloud-native workload identity token verified
  inside the handler itself, since three clouds mean three issuers and no
  single-issuer JWT authorizer fits), and the auto-deploying `$default` stage
  with throttle limits and access logging to the API log group. These four
  are one step because they are meaningless apart — an API with no route is
  a broken endpoint, not a partial one. Exposes `endpoint()` (full status-push
  URL, empty until the API exists) and `executeARN()` (used to scope the
  Lambda invoke permission).
- **`permissionStep`** (`step_lambda.go`) — grants API Gateway
  (`apigateway.amazonaws.com`) permission to invoke the ingestion function,
  scoped to this one API's execute-api ARN via a stable statement id
  (`AllowInvokeFromIngestionApi`) so re-running converge recognises the
  permission it added last time instead of stacking duplicates. Holds a
  reference to the `*apiStep` rather than a resolved API id, since the id only
  exists once that step has run. Treats `ResourceConflictException` from
  `AddPermission` as already-converged, not a failure.

## Constants

- `DefaultNamePrefix = "kubespin"`, `DefaultLogRetentionDays = 30`,
  `DefaultThrottleBurst = 100`, `DefaultThrottleRate = 50`.
- `StatusRouteKey = "POST /v1/clusters/{clusterId}/status"` — the only route
  on the ingestion API; the `{clusterId}` in the path is what M6 binds the
  caller's token subject against.
