# internal/auth

`internal/auth` authenticates the operator to every cloud kubespin talks to by
shelling out to the same CLIs (`aws`, `gcloud`, `az`) an operator would use by
hand, rather than keeping its own credential store. Login sessions live
wherever those CLIs already cache them (`~/.aws/sso/cache`,
`~/.config/gcloud`, `~/.azure`), so kubespin has nothing extra to secure or
expire — this is why cloud auth in kubespin is CLI-session-based, not
env-var-based. `internal/cli` (`login.go`) builds a `Registry` of one
`AWSProvider`, one `GCPProvider`, and one `AzureProvider` and uses it to back
three commands — `kubespin login`, `kubespin status`, `kubespin logout` — and
a shared `ensureAuthenticated` preflight that every `apply`/`delete` command
runs before touching a cloud SDK, so a missing session fails fast with "run
kubespin login" instead of a cryptic SDK error mid-provision.

## Types

### `Provider` (auth.go)

Interface every cloud's authentication surface implements. Reached uniformly
by the orchestrator functions and by every command that needs credentials, so
none of them branch on which cloud they're talking to — adding a new cloud is
a new file implementing this, not a change to `login`/`status`/`logout`.

```go
type Provider interface {
    Name() string
    IsAuthenticated(ctx context.Context) (bool, StatusDetail, error)
    Login(ctx context.Context) error
    Logout(ctx context.Context) error
}
```

- `Name() string` — identifies the provider for `--only`, status output, and error messages (e.g. `"aws"`).
- `IsAuthenticated(ctx) (bool, StatusDetail, error)` — makes a real call (not just "does a token file exist") so a stale or revoked session is reported accurately. A `false` result is *not* itself an error — `err` is reserved for failures unrelated to auth state, such as the provider's CLI missing from PATH.
- `Login(ctx) error` — authenticates interactively (typically opens a browser); blocks until the flow completes or fails.
- `Logout(ctx) error` — clears the provider's cached session.

Implemented by `AWSProvider`, `GCPProvider`, and `AzureProvider`.

### `StatusDetail` (auth.go)

What a `Provider` reports about its own session beyond the plain
authenticated/not-authenticated bit.

| Field | Type | Meaning |
|---|---|---|
| `Message` | `string` | Short human-readable summary, e.g. `"4 accounts reachable"` or `"logged in as you@org.com"`. |
| `ExpiresAt` | `*time.Time` | When the current session expires, if determinable. `nil` means unknown/not applicable — **not** "never expires". |

### `Result` (auth.go)

The outcome of running one operation against one provider. Every orchestrator
function (`Status`, `Login`, `Logout`) returns a slice of these — one per
provider, in the same order the providers were given — rather than failing
the whole batch on the first error, so a `login` run against three clouds
still reports all three even if one fails.

| Field | Type |
|---|---|
| `Provider` | `string` |
| `Authenticated` | `bool` |
| `Status` | `StatusDetail` |
| `Err` | `error` |

### `Registry` (auth.go)

Holds every configured provider, in the order `status`/`login`/`logout`
report them.

```go
type Registry struct {
    providers []Provider
}
```

Methods:

- `NewRegistry(providers ...Provider) *Registry` — builds a registry over the given providers, in report order.
- `(*Registry) Select(names []string) ([]Provider, error)` — returns the providers named (case-insensitively), in registry order, or an error naming any name that matched nothing. An empty `names` list selects every provider — this is what backs `--only`.

### `Option` / `options` (auth.go)

`options` carries settings shared by every provider constructor in the
package, so a caller configures all three clouds the same way. `Option` is a
functional option (`func(*options)`) over it.

- `WithLogger(logger *slog.Logger) Option` — sets the logger. Without it, a provider logs to `slog.Default()`. Logging in this package is Debug-level on purpose: `kubespin login`/`status` already report per-provider outcomes to the operator, so this is only the "which CLI did we actually shell out to" detail needed when diagnosing a problem.

### `AWSProvider` (aws.go)

Authenticates via AWS IAM Identity Center (SSO) — the same flow documented in
`docs/fleet-bootstrap.md` for the Fleet Registry account.

```go
type AWSProvider struct {
    profile string
    sts     stsAPI
    run     commandRunner
    logger  *slog.Logger
}
```

- `profile` — the named profile in `~/.aws/config` this provider is scoped to.
- `sts` — narrowed to `stsAPI`'s single method (`GetCallerIdentity`), the same narrowing pattern `internal/fleetinfra`'s client interfaces use, so `IsAuthenticated` is testable without real AWS credentials.

Invariant: constructing an `AWSProvider` succeeds even before the operator has ever logged in or run `aws configure` — a missing `"default"` profile section falls back to the SDK's unscoped default resolution, but a named profile that doesn't exist still errors since the operator explicitly asked for it.

### `AzureProvider` (azure.go)

Authenticates via the `az` CLI. `internal/provisioner/azure`'s
`NewDefaultAzureCredential` falls back to exactly this cached session once no
environment/managed-identity credential is available, so a logged-in `az` CLI
is what makes kubespin's own Azure client construction work.

```go
type AzureProvider struct {
    run     commandRunner
    newCred func() (tokenCredential, error)
    logger  *slog.Logger
}
```

`newCred` is a factory over `azidentity.NewAzureCLICredential`, narrowed to
the `tokenCredential` interface (`GetToken`) for testability — the same
pattern as `stsAPI`.

Invariant: `IsAuthenticated` requests a real management-plane token
(`azureManagementScope = "https://management.azure.com/.default"`) rather than
just checking `az account show`, so an expired or revoked session is reported
accurately.

### `GCPProvider` (gcp.go)

Authenticates via `gcloud`: a user login (for interactive/CLI use) plus
Application Default Credentials (what the GCP SDK clients in
`internal/provisioner/gcp` actually read).

```go
type GCPProvider struct {
    run    commandRunner
    out    commandOutput
    logger *slog.Logger
}
```

Invariant: `Login`/`Logout` always perform *both* the user login/revoke and
the Application Default Credentials login/revoke — skipping the ADC leg is
the classic "gcloud works but my Go program can't authenticate" trap.

### `commandRunner` / `commandOutput` (auth.go)

Abstract shelling out to a CLI, so `Login`/`Logout`/`IsAuthenticated` are
testable without actually invoking `aws`/`gcloud`/`az`.

```go
type commandRunner func(ctx context.Context, name string, args ...string) error
type commandOutput func(ctx context.Context, name string, args ...string) (string, error)
```

`commandRunner` (`execRunner`) is for interactive flows with the operator's
own stdio attached (`aws sso login`, `gcloud auth login`, `az login` — all
print a code or open a browser). `commandOutput` (`execOutput`) is for checks
that need a captured value back (an account name, a token).

### `stsAPI` (aws.go) / `tokenCredential` (azure.go)

Narrow single-method interfaces (`GetCallerIdentity`, `GetToken`
respectively) over the AWS STS client and Azure `azidentity` credential, each
existing solely to make its provider's `IsAuthenticated` testable without
live cloud credentials.

## Functions

### `Status`

```go
func Status(ctx context.Context, providers []Provider) []Result
```

Checks every provider concurrently. Has no side effects and never returns an
error itself — per-provider failures are carried in each `Result` — so it is
safe to run as often as a caller likes, including as a preflight before every
cloud-calling command.

### `Login`

```go
func Login(ctx context.Context, providers []Provider, force bool) []Result
```

Authenticates every provider concurrently — each may pop open a browser and
there is no dependency between them, so running them one at a time would just
be a needless wait. A provider whose session already looks valid is left
alone unless `force` is set. After `Login` runs, it re-checks
`IsAuthenticated` and reports that as the result's authenticated state.

### `Logout`

```go
func Logout(ctx context.Context, providers []Provider) []Result
```

Clears every provider's cached session concurrently.

### `EnsureAll`

```go
func EnsureAll(ctx context.Context, providers []Provider) error
```

The preflight every command that calls a cloud SDK should run before it does
anything else. Runs `Status` and, if any provider is not authenticated,
returns an error naming all of them (sorted) and pointing at
`kubespin login --only <providers>`, rather than surfacing a cryptic SDK auth
error partway through provisioning. Returns `nil` if every provider is
authenticated.

### `NewRegistry`

```go
func NewRegistry(providers ...Provider) *Registry
```

Builds a `Registry` over the given providers, in report order.

### `NewAWSProvider`

```go
func NewAWSProvider(ctx context.Context, profile string, opts ...Option) (*AWSProvider, error)
```

Builds a provider scoped to one named profile in `~/.aws/config`. An empty
`profile` defaults to `"default"`. See the `AWSProvider` invariant above for
the fallback behavior on a missing default profile.

### `NewAzureProvider`

```go
func NewAzureProvider(opts ...Option) *AzureProvider
```

Builds a provider over the `az` CLI's cached session.

### `NewGCPProvider`

```go
func NewGCPProvider(opts ...Option) *GCPProvider
```

Builds a provider that shells out to the `gcloud` CLI.

### `WithLogger`

```go
func WithLogger(logger *slog.Logger) Option
```

Sets the logger used by a provider constructor. A `nil` logger is ignored
(the provider keeps `slog.Default()`).

### `WriteTable` (status.go)

```go
func WriteTable(w io.Writer, results []Result)
```

Renders `Result`s the way `login`/`status`/`logout` show them: one line per
provider via `tabwriter`, a `✓`/`✗` mark, and whatever detail
`IsAuthenticated` reported. Behavior:

- `Err != nil` → `✗`, detail is the error text.
- not authenticated and no error → `✗`, detail defaults to `"not authenticated — run: kubespin login --only <provider>"` if `Status.Message` is empty.
- authenticated with `Status.ExpiresAt` set → detail is suffixed with `"(session expires in <duration>)"`.

### Internal helpers (auth.go)

Unexported but relevant to how the package is wired:

- `resolve(opts []Option) options` — applies `Option`s over the defaults (`logger: slog.Default()`).
- `loggerOr(logger *slog.Logger) *slog.Logger` — returns `slog.Default()` if `logger` is `nil`, so a provider built as a bare struct literal (as this package's tests do) doesn't panic.
- `run(providers []Provider, work func(Provider) Result) []Result` — fans work out across providers concurrently via `errgroup.Group`, collecting one `Result` per provider in original order. The worker never returns an error to the errgroup — a provider failure belongs in its own `Result`, not in stopping the other providers' work early. Backs `Status`, `Login`, and `Logout`.
- `execRunner` / `execOutput` — the real `commandRunner`/`commandOutput` implementations, built on `exec.CommandContext`.
- `checkBinary(name, installHint string) error` — reports a clear, actionable error when a provider's CLI isn't on PATH, rather than letting `exec.Command` fail with a raw "executable file not found" once `Login`/`Logout` tries to run it.
