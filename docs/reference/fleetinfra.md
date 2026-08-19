# internal/fleetinfra

`internal/fleetinfra` is the SDK converge engine behind `kubespin fleet bootstrap`. It provisions the shared, once-per-fleet-account infrastructure directly through `aws-sdk-go-v2` — no Terraform, no CloudFormation, no state file. It creates the ingestion Lambda's execution role and log groups, the ingestion Lambda function itself, and the Central Ingestion API (API Gateway v2 + Lambda proxy integration). The Fleet Registry itself is **not** provisioned here: it is a Postgres database (`internal/registry`) the operator provisions and supplies a connection string for via `KUBESPIN_REGISTRY_DSN` — it self-migrates its own schema on first connect, so there is nothing for this package to create for it.

Because there is no state file, convergence is the contract: every step describes live AWS state, diffs it against `Spec`, and is create-or-update — nothing ever deletes. `Plan` is strictly read-only (what `--dry-run` runs); a run against already-provisioned infrastructure must report zero changes.

## Quick reference

| Name | Kind | File | Summary |
|---|---|---|---|
| [`Spec`](#spec) | Type | fleetinfra.go | Desired state of the fleet infrastructure, passed to `Converge`. |
| [`ActionKind`](#actionkind) | Type | fleetinfra.go | Enum of a step's plan intent: none/create/update. |
| [`Action`](#action) | Type | fleetinfra.go | One step's verdict on one resource. |
| [`Option`](#option-withlogger) / [`WithLogger`](#option-withlogger) | Type / Func | fleetinfra.go | Functional option for configuring a `Converge` run. |
| [`Report`](#report) | Type | fleetinfra.go | Outcome of a converge run. |
| [`ErrSpec`](#errors) | Var | fleetinfra.go | Wraps configuration/validation problems. |
| [`ErrAccountMismatch`](#errors) | Var | fleetinfra.go | Returned when caller account != configured fleet account. |
| [`DefaultNamePrefix`, `DefaultLogRetentionDays`, `DefaultThrottleBurst`, `DefaultThrottleRate`](#constants) | Const | fleetinfra.go | Defaults filled into an unset `Spec`. |
| [`StatusRouteKey`](#constants) | Const | fleetinfra.go | The one route on the ingestion API. |
| [`Converge`](#converge) | Func | fleetinfra.go | Brings fleet infrastructure to match `spec`. |
| [`Clients`](#clients) | Type | clients.go | Bundles the six AWS service interfaces used by converge steps. |
| [`NewClients`](#newclients) | Func | clients.go | Constructs real AWS SDK clients from the ambient credential chain. |
| [`PackageLambda`](#packagelambda) | Func | package.go | Packages the compiled ingestion handler binary into a deterministic zip. |

## fleetinfra.go

### `Spec`

??? abstract "Signature"

    ```go
    type Spec struct {
        AccountID string
        Region    string

        NamePrefix       string
        RegistryDSN      string
        LogRetentionDays int32
        ThrottleBurst    int32
        ThrottleRate     float64

        LambdaZip []byte
    }
    ```

    - **Fields:**
        - `AccountID` — the fleet account; checked against the caller's real STS identity before anything is provisioned (`ErrAccountMismatch` on mismatch).
        - `RegistryDSN` — the Fleet Registry's Postgres connection string, passed straight through as the ingestion Lambda's `REGISTRY_DSN` environment variable (`functionStep`); never used to provision anything, since the database itself is out of scope for this package.
        - `LambdaZip` — the packaged ingestion handler produced by `PackageLambda`.
        - Unset tunables (`NamePrefix`, `LogRetentionDays`, `ThrottleBurst`, `ThrottleRate`) are filled from the `Default*` constants by the unexported `withDefaults` method before use.
    - **Behavior:** `Validate() error` reports every problem at once (via `errors.Join`): account id must be 12 digits, `Region` required, `RegistryDSN` required, `LambdaZip` non-empty — all wrapping `ErrSpec`.
    - **Invariants:** Unexported helper methods derive resource names/ARNs from the spec (`functionName`, `roleName`, `apiName`, `lambdaLogGroup`, `apiLogGroup`, `roleARN`, `functionARN`, `invokeARN`, `lambdaLogGroupARN`, `apiLogGroupARN` — no `tableARN`, since there is no table); the partition is assumed to be `aws` — GovCloud/China would need this threaded through the spec.

### `ActionKind`

??? abstract "Signature"

    ```go
    type ActionKind int

    const (
        ActionNone ActionKind = iota
        ActionCreate
        ActionUpdate
    )
    ```

    - **Behavior:** Enum of what a step's plan intends to do. There is no `ActionDelete` — `Converge` never deletes. `String()` renders `"in sync"`, `"create"`, `"update"`.

### `Action`

??? abstract "Signature"

    ```go
    type Action struct {
        Resource string
        Kind     ActionKind
        Details  []string
    }
    ```

    - **Fields:** `Details` explains what differs and is printed on both dry and real runs.
    - **Behavior:** `String()` formats as `"<resource>  <kind> (<details>)"`.

### `Option` / `WithLogger`

??? abstract "Signature"

    ```go
    type Option func(*options)

    func WithLogger(logger *slog.Logger) Option
    ```

    - **Behavior:** `Option` is a functional option for configuring a `Converge` run; `WithLogger` sets the `*slog.Logger` the run narrates itself through. A nil logger is ignored, leaving `slog.Default()` in place.

### `Report`

??? abstract "Signature"

    ```go
    type Report struct {
        DryRun       bool
        Actions      []Action
        IngestionURL string
    }
    ```

    - **Fields:** `IngestionURL` is the full endpoint clusters push status to (base API endpoint + `/v1/clusters/{clusterId}/status`), and the host every cluster's egress allowlist must permit. Empty on a dry run that would still have to create the API.
    - **Behavior:** `Changed() int` counts actions whose `Kind != ActionNone`.

### Errors

??? note "Signature"

    ```go
    var (
        ErrSpec            = errors.New("invalid fleet infrastructure spec")
        ErrAccountMismatch = errors.New("caller account does not match the configured fleet account")
    )
    ```

    - **Behavior:** Both are wrapped (`fmt.Errorf("%w: ...")`) rather than returned bare, so callers can match with `errors.Is`.

### Constants

??? note "Signature"

    ```go
    const (
        DefaultNamePrefix       = "kubespin"
        DefaultLogRetentionDays = 30
        DefaultThrottleBurst    = 100
        DefaultThrottleRate     = 50

        StatusRouteKey = "POST /v1/clusters/{clusterId}/status"
    )
    ```

    - **Behavior:** `Default*` constants back-fill unset `Spec` tunables. `StatusRouteKey` is the only route on the ingestion API; the `{clusterId}` in the path is what M6 binds the caller's token subject against.

### `Converge`

??? abstract "Signature"

    ```go
    func Converge(ctx context.Context, c *Clients, spec Spec, dryRun bool, opts ...Option) (Report, error)
    ```

    - **Behavior:** Brings the fleet infrastructure to match `spec`. Applies `spec.withDefaults()`, validates it, and verifies the caller's account before touching anything. Runs five steps in dependency order — log groups, IAM role, Lambda function, ingestion API, invoke permission — stopping at the first error, so a failure leaves earlier resources created and later ones untouched; re-running resumes, since every step is create-or-update. There is no registry-table step: the Fleet Registry is a Postgres database provisioned outside this package.
    - **Invariants:** For each step, `Plan` runs first (always, dry or real) and its `Action` is appended to `Report.Actions`. If the action is `ActionNone`, the step is logged at Debug and skipped. On a dry run, a would-be change is logged at Info and *not* applied. On a real run, `Apply` is called and any error aborts the whole `Converge` call. `Report.IngestionURL` is populated from the API step's resolved endpoint before returning.

## clients.go

Each AWS service is reached through a narrow interface listing only the calls the package makes. This is what lets the whole converge engine be unit-tested without credentials, and documents the exact blast radius of the permissions a bootstrap operator needs. All five are unexported and bundled into `*Clients`, built for real use by `NewClients`. There is deliberately no registry-backing interface here (no `dynamoAPI` equivalent, e.g. a `postgresAPI`) — Postgres reachability is a network/security-group concern for the operator, not something this AWS SDK converge engine mediates.

| Interface | Methods | Implementer |
|---|---|---|
| `stsAPI` | `GetCallerIdentity` | `sts.Client` (`github.com/aws/aws-sdk-go-v2/service/sts`) |
| `logsAPI` | `DescribeLogGroups`, `CreateLogGroup`, `PutRetentionPolicy` | `cloudwatchlogs.Client` |
| `iamAPI` | `GetRole`, `CreateRole`, `GetRolePolicy`, `PutRolePolicy` | `iam.Client` |
| `lambdaAPI` | `GetFunction`, `CreateFunction`, `UpdateFunctionCode`, `UpdateFunctionConfiguration`, `GetPolicy`, `AddPermission` | `lambda.Client` |
| `apiGatewayAPI` | `GetApis`, `CreateApi`, `GetIntegrations`, `CreateIntegration`, `GetRoutes`, `CreateRoute`, `GetStage`, `CreateStage`, `UpdateStage` | `apigatewayv2.Client` |

### `Clients`

??? abstract "Signature"

    ```go
    type Clients struct {
        // unexported: sts, logs, iam, lambda, apiGateway
    }
    ```

    - **Behavior:** Bundles the five AWS service interfaces that converge steps use. All fields are unexported; construct via `NewClients`.
    - **Invariants:** `(*Clients).verifyAccount(ctx, want string) error` is the guard that replaces Terraform's `allowed_account_ids` — it calls `GetCallerIdentity` and refuses to provision if the caller's account doesn't match `want`, which is what keeps fleet infrastructure out of a cluster account.

### `NewClients`

??? abstract "Signature"

    ```go
    func NewClients(ctx context.Context, region string) (*Clients, error)
    ```

    - **Behavior:** Loads the ambient AWS credential chain via `config.LoadDefaultConfig` scoped to `region`, and constructs real `sts`/`cloudwatchlogs`/`iam`/`lambda`/`apigatewayv2` clients from it.

## package.go

### `PackageLambda`

??? abstract "Signature"

    ```go
    func PackageLambda(binaryPath string) ([]byte, error)
    ```

    - **Behavior:** Opens the compiled handler binary at `binaryPath` and returns a deployable zip (delegates to the unexported `packageBinary`).
    - **Invariants:** The zip contains exactly one entry, named `bootstrap` (the `handlerName` constant — what `provided.al2023` executes), mode `0o755`, deflated. Every entry's modified timestamp is pinned to `zipEpoch` (1980-01-01 UTC): determinism is load-bearing, because convergence in `functionStep.Plan` compares the archive's SHA-256 against the deployed function's `CodeSha256`, and an archive carrying the current time would hash differently on every run and report drift forever.

## Steps (unexported, one per file)

Each step implements the internal `step` interface (`Name() string`, `Plan(ctx) (Action, error)`, `Apply(ctx, Action) error`) and is constructed by an unexported `newXStep` function taking `(*Clients, Spec, *slog.Logger)`. There are five, one fewer than before this package's registry migration: `step_registry.go` and its `registryTableStep` were deleted outright rather than repointed at Postgres, since a Postgres database is not an AWS resource this SDK-driven converge engine has any business provisioning — the operator provisions it themselves and `kubespin` connects via `RegistryDSN`/`KUBESPIN_REGISTRY_DSN`.

### `logGroupsStep`

??? note "Signature"

    ```go
    // step_logs.go
    func newLogGroupsStep(c *Clients, spec Spec, logger *slog.Logger) *logGroupsStep
    ```

    - **Behavior:** Provisions both the Lambda's and the API's CloudWatch log groups up front, so retention (`LogRetentionDays`) can be set at all (an implicitly-created group retains forever) and so the Lambda's execution policy can be scoped to a group that already exists.

### `roleStep`

??? note "Signature"

    ```go
    // step_iam.go
    func newRoleStep(c *Clients, spec Spec, logger *slog.Logger) *roleStep
    ```

    - **Behavior:** Provisions the ingestion Lambda's execution IAM role, with a deliberately tiny inline policy (`ingestion`): only `logs:CreateLogStream`/`PutLogEvents` on its own log group. No registry-access statement at all — unlike the DynamoDB-backed registry this replaced, reaching Postgres is over the network via `REGISTRY_DSN`, not an IAM-mediated AWS API, so there is nothing for this policy to grant.
    - **Invariants:** Policy equality is checked semantically (`policyEqual`, via JSON round-trip) against a policy IAM may have reformatted.

### `functionStep`

??? note "Signature"

    ```go
    // step_lambda.go
    func newFunctionStep(c *Clients, spec Spec, logger *slog.Logger) *functionStep
    ```

    - **Behavior:** Provisions the ingestion Lambda: `provided.al2023` runtime, arm64, 10s timeout, 256MB memory, one env var `REGISTRY_DSN` (never logged, since it carries the Postgres password).
    - **Invariants:** Drift on code is detected by comparing the deployed `CodeSha256` against `codeSHA256(spec.LambdaZip)` (`package.go`), which is why the zip must be byte-deterministic.

### `apiStep`

??? note "Signature"

    ```go
    // step_api.go
    func newAPIStep(c *Clients, spec Spec, logger *slog.Logger) *apiStep

    func (s *apiStep) endpoint() string
    func (s *apiStep) executeARN() string
    ```

    - **Behavior:** Provisions the Central Ingestion API as one step covering the HTTP API, its AWS_PROXY Lambda integration, the single `POST /v1/clusters/{clusterId}/status` route (`AuthorizationType: NONE` — the caller authenticates via a cloud-native workload identity token verified inside the handler itself, since three clouds mean three issuers and no single-issuer JWT authorizer fits), and the auto-deploying `$default` stage with throttle limits and access logging to the API log group.
    - **Invariants:** These four are one step because they are meaningless apart — an API with no route is a broken endpoint, not a partial one. Exposes `endpoint()` (full status-push URL, empty until the API exists) and `executeARN()` (used to scope the Lambda invoke permission).

### `permissionStep`

??? note "Signature"

    ```go
    // step_lambda.go
    func newPermissionStep(c *Clients, spec Spec, api *apiStep, logger *slog.Logger) *permissionStep
    ```

    - **Behavior:** Grants API Gateway (`apigateway.amazonaws.com`) permission to invoke the ingestion function, scoped to this one API's execute-api ARN via a stable statement id (`AllowInvokeFromIngestionApi`) so re-running converge recognises the permission it added last time instead of stacking duplicates.
    - **Invariants:** Holds a reference to the `*apiStep` rather than a resolved API id, since the id only exists once that step has run. Treats `ResourceConflictException` from `AddPermission` as already-converged, not a failure.
