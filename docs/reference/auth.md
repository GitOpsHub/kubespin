# internal/auth

`internal/auth` authenticates the operator to every cloud kubespin talks to by shelling out to the same CLIs (`aws`, `gcloud`, `az`) an operator would use by hand, rather than keeping its own credential store. Login sessions live wherever those CLIs already cache them (`~/.aws/sso/cache`, `~/.config/gcloud`, `~/.azure`), so kubespin has nothing extra to secure or expire — this is why cloud auth in kubespin is CLI-session-based, not env-var-based. `internal/cli` (`login.go`) builds a `Registry` of one `AWSProvider`, one `GCPProvider`, and one `AzureProvider` and uses it to back three commands — `kubespin login`, `kubespin status`, `kubespin logout` — and a shared `ensureAuthenticated` preflight that every `apply`/`delete` command runs before touching a cloud SDK.

## Quick reference

| Name | Kind | File | Summary |
|---|---|---|---|
| [Provider](#provider) | interface | auth.go | Uniform authentication surface implemented by every cloud provider |
| [StatusDetail](#statusdetail) | struct | auth.go | Session detail (message + expiry) reported by a `Provider` |
| [Result](#result) | struct | auth.go | Outcome of one operation against one provider |
| [Registry](#registry) | struct | auth.go | Holds every configured provider in report order |
| [Option](#option--options) | func type | auth.go | Functional option over provider constructor settings |
| [WithLogger](#withlogger) | func | auth.go | Sets the logger used by a provider constructor |
| [NewRegistry](#newregistry) | func | auth.go | Builds a `Registry` over the given providers |
| [Status](#status) | func | auth.go | Concurrently checks every provider's auth state |
| [Login](#login) | func | auth.go | Concurrently authenticates every provider |
| [Logout](#logout) | func | auth.go | Concurrently clears every provider's cached session |
| [EnsureAll](#ensureall) | func | auth.go | Preflight that errors fast if any provider is unauthenticated |
| [commandRunner / commandOutput](#commandrunner--commandoutput) | func type | auth.go | Abstractions over shelling out to a CLI |
| [AWSProvider](#awsprovider) | struct | aws.go | Authenticates via AWS IAM Identity Center (SSO) |
| [NewAWSProvider](#newawsprovider) | func | aws.go | Builds a provider scoped to one named AWS profile |
| [stsAPI](#stsapi) | interface | aws.go | Narrowed AWS STS client interface for testability |
| [AzureProvider](#azureprovider) | struct | azure.go | Authenticates via the `az` CLI |
| [NewAzureProvider](#newazureprovider) | func | azure.go | Builds a provider over the `az` CLI's cached session |
| [tokenCredential](#tokencredential) | interface | azure.go | Narrowed Azure `azidentity` credential interface for testability |
| [GCPProvider](#gcpprovider) | struct | gcp.go | Authenticates via the `gcloud` CLI (user login + ADC) |
| [NewGCPProvider](#newgcpprovider) | func | gcp.go | Builds a provider that shells out to the `gcloud` CLI |
| [WriteTable](#writetable) | func | status.go | Renders `Result`s as a `✓`/`✗` table for the CLI |

## auth.go

??? abstract "Signature — `Provider` interface"

    ```go
    type Provider interface {
        Name() string
        IsAuthenticated(ctx context.Context) (bool, StatusDetail, error)
        Login(ctx context.Context) error
        Logout(ctx context.Context) error
    }
    ```

    - **Fields/Params:**
        - `Name() string` — identifies the provider for `--only`, status output, and error messages (e.g. `"aws"`).
        - `IsAuthenticated(ctx) (bool, StatusDetail, error)` — makes a real call (not just "does a token file exist") so a stale or revoked session is reported accurately. A `false` result is *not* itself an error — `err` is reserved for failures unrelated to auth state, such as the provider's CLI missing from PATH.
        - `Login(ctx) error` — authenticates interactively (typically opens a browser); blocks until the flow completes or fails.
        - `Logout(ctx) error` — clears the provider's cached session.
    - **Behavior:** reached uniformly by the orchestrator functions and by every command that needs credentials, so none of them branch on which cloud they're talking to.
    - **Invariants:** implemented by `AWSProvider`, `GCPProvider`, and `AzureProvider`; adding a new cloud is a new file implementing this interface, not a change to `login`/`status`/`logout`.

??? abstract "Signature — `StatusDetail` struct"

    ```go
    type StatusDetail struct {
        Message   string
        ExpiresAt *time.Time
    }
    ```

    - **Fields/Params:**
        - `Message` — short human-readable summary, e.g. `"4 accounts reachable"` or `"logged in as you@org.com"`.
        - `ExpiresAt` — when the current session expires, if determinable.
    - **Behavior:** what a `Provider` reports about its own session beyond the plain authenticated/not-authenticated bit.
    - **Invariants:** `ExpiresAt == nil` means unknown/not applicable — **not** "never expires".

??? abstract "Signature — `Result` struct"

    ```go
    type Result struct {
        Provider      string
        Authenticated bool
        Status        StatusDetail
        Err           error
    }
    ```

    - **Behavior:** the outcome of running one operation against one provider; `Status`/`Login`/`Logout` each return a slice of these — one per provider, in the same order the providers were given — rather than failing the whole batch on the first error.
    - **Invariants:** a `login` run against three clouds still reports all three even if one fails.

??? abstract "Signature — `Registry` struct"

    ```go
    type Registry struct {
        providers []Provider
    }
    ```

    - **Behavior:** holds every configured provider, in the order `status`/`login`/`logout` report them.
    - Methods:
        - `NewRegistry(providers ...Provider) *Registry` — builds a registry over the given providers, in report order.
        - `(*Registry) Select(names []string) ([]Provider, error)` — returns the providers named (case-insensitively), in registry order, or an error naming any name that matched nothing. An empty `names` list selects every provider — this is what backs `--only`.

??? abstract "Signature — `Option` / `options`"

    ```go
    type options struct {
        logger *slog.Logger
    }
    type Option func(*options)
    ```

    - **Behavior:** `options` carries settings shared by every provider constructor in the package, so a caller configures all three clouds the same way; `Option` is a functional option over it.

??? note "Signature — `WithLogger`"

    ```go
    func WithLogger(logger *slog.Logger) Option
    ```

    - **Behavior:** sets the logger used by a provider constructor. Without it, a provider logs to `slog.Default()`.
    - **Invariants:** a `nil` logger is ignored (the provider keeps `slog.Default()`); logging in this package is Debug-level on purpose since `kubespin login`/`status` already report per-provider outcomes to the operator — this is only the "which CLI did we actually shell out to" detail needed when diagnosing a problem.

??? note "Signature — `NewRegistry`"

    ```go
    func NewRegistry(providers ...Provider) *Registry
    ```

    - **Behavior:** builds a `Registry` over the given providers, in report order.

??? note "Signature — `Status`"

    ```go
    func Status(ctx context.Context, providers []Provider) []Result
    ```

    - **Behavior:** checks every provider concurrently.
    - **Invariants:** has no side effects and never returns an error itself — per-provider failures are carried in each `Result` — so it is safe to run as often as a caller likes, including as a preflight before every cloud-calling command.

??? note "Signature — `Login`"

    ```go
    func Login(ctx context.Context, providers []Provider, force bool) []Result
    ```

    - **Behavior:** authenticates every provider concurrently — each may pop open a browser and there is no dependency between them, so running them one at a time would just be a needless wait. A provider whose session already looks valid is left alone unless `force` is set. After `Login` runs, it re-checks `IsAuthenticated` and reports that as the result's authenticated state.

??? note "Signature — `Logout`"

    ```go
    func Logout(ctx context.Context, providers []Provider) []Result
    ```

    - **Behavior:** clears every provider's cached session concurrently.

??? note "Signature — `EnsureAll`"

    ```go
    func EnsureAll(ctx context.Context, providers []Provider) error
    ```

    - **Behavior:** the preflight every command that calls a cloud SDK should run before it does anything else. Runs `Status` and, if any provider is not authenticated, returns an error naming all of them (sorted) and pointing at `kubespin login --only <providers>`, rather than surfacing a cryptic SDK auth error partway through provisioning.
    - **Invariants:** returns `nil` if every provider is authenticated.

??? abstract "Signature — `commandRunner` / `commandOutput`"

    ```go
    type commandRunner func(ctx context.Context, name string, args ...string) error
    type commandOutput func(ctx context.Context, name string, args ...string) (string, error)
    ```

    - **Behavior:** abstract shelling out to a CLI, so `Login`/`Logout`/`IsAuthenticated` are testable without actually invoking `aws`/`gcloud`/`az`. `commandRunner` (`execRunner`) is for interactive flows with the operator's own stdio attached (`aws sso login`, `gcloud auth login`, `az login` — all print a code or open a browser). `commandOutput` (`execOutput`) is for checks that need a captured value back (an account name, a token).

??? note "Internal helpers"

    Unexported but relevant to how the package is wired:

    - `resolve(opts []Option) options` — applies `Option`s over the defaults (`logger: slog.Default()`).
    - `loggerOr(logger *slog.Logger) *slog.Logger` — returns `slog.Default()` if `logger` is `nil`, so a provider built as a bare struct literal (as this package's tests do) doesn't panic.
    - `run(providers []Provider, work func(Provider) Result) []Result` — fans work out across providers concurrently via `errgroup.Group`, collecting one `Result` per provider in original order. The worker never returns an error to the errgroup — a provider failure belongs in its own `Result`, not in stopping the other providers' work early. Backs `Status`, `Login`, and `Logout`.
    - `execRunner` / `execOutput` — the real `commandRunner`/`commandOutput` implementations, built on `exec.CommandContext`.
    - `checkBinary(name, installHint string) error` — reports a clear, actionable error when a provider's CLI isn't on PATH, rather than letting `exec.Command` fail with a raw "executable file not found" once `Login`/`Logout` tries to run it.

## aws.go

??? abstract "Signature — `AWSProvider` struct"

    ```go
    type AWSProvider struct {
        profile string
        sts     stsAPI
        run     commandRunner
        logger  *slog.Logger
    }
    ```

    - **Fields/Params:**
        - `profile` — the named profile in `~/.aws/config` this provider is scoped to.
        - `sts` — narrowed to `stsAPI`'s single method (`GetCallerIdentity`), the same narrowing pattern `internal/fleetinfra`'s client interfaces use, so `IsAuthenticated` is testable without real AWS credentials.
    - **Behavior:** authenticates via AWS IAM Identity Center (SSO) — the same flow documented in `docs/fleet-bootstrap.md` for the Fleet Registry account.
    - **Invariants:** constructing an `AWSProvider` succeeds even before the operator has ever logged in or run `aws configure` — a missing `"default"` profile section falls back to the SDK's unscoped default resolution, but a named profile that doesn't exist still errors since the operator explicitly asked for it.

??? note "Signature — `NewAWSProvider`"

    ```go
    func NewAWSProvider(ctx context.Context, profile string, opts ...Option) (*AWSProvider, error)
    ```

    - **Behavior:** builds a provider scoped to one named profile in `~/.aws/config`. An empty `profile` defaults to `"default"`.
    - **Invariants:** see the `AWSProvider` invariant above for the fallback behavior on a missing default profile.

??? abstract "Signature — `stsAPI` interface"

    ```go
    type stsAPI interface {
        GetCallerIdentity(...) (...)
    }
    ```

    - **Behavior:** narrow single-method interface over the AWS STS client, existing solely to make `AWSProvider.IsAuthenticated` testable without live cloud credentials.

## azure.go

??? abstract "Signature — `AzureProvider` struct"

    ```go
    type AzureProvider struct {
        run     commandRunner
        newCred func() (tokenCredential, error)
        logger  *slog.Logger
    }
    ```

    - **Fields/Params:** `newCred` is a factory over `azidentity.NewAzureCLICredential`, narrowed to the `tokenCredential` interface (`GetToken`) for testability — the same pattern as `stsAPI`.
    - **Behavior:** authenticates via the `az` CLI. `internal/provisioner/azure`'s `NewDefaultAzureCredential` falls back to exactly this cached session once no environment/managed-identity credential is available, so a logged-in `az` CLI is what makes kubespin's own Azure client construction work.
    - **Invariants:** `IsAuthenticated` requests a real management-plane token (`azureManagementScope = "https://management.azure.com/.default"`) rather than just checking `az account show`, so an expired or revoked session is reported accurately.

??? note "Signature — `NewAzureProvider`"

    ```go
    func NewAzureProvider(opts ...Option) *AzureProvider
    ```

    - **Behavior:** builds a provider over the `az` CLI's cached session.

??? abstract "Signature — `tokenCredential` interface"

    ```go
    type tokenCredential interface {
        GetToken(...) (...)
    }
    ```

    - **Behavior:** narrow single-method interface over the Azure `azidentity` credential, existing solely to make `AzureProvider.IsAuthenticated` testable without live cloud credentials.

## gcp.go

??? abstract "Signature — `GCPProvider` struct"

    ```go
    type GCPProvider struct {
        run    commandRunner
        out    commandOutput
        logger *slog.Logger
    }
    ```

    - **Behavior:** authenticates via `gcloud`: a user login (for interactive/CLI use) plus Application Default Credentials (what the GCP SDK clients in `internal/provisioner/gcp` actually read).
    - **Invariants:** `Login`/`Logout` always perform *both* the user login/revoke and the Application Default Credentials login/revoke — skipping the ADC leg is the classic "gcloud works but my Go program can't authenticate" trap.

??? note "Signature — `NewGCPProvider`"

    ```go
    func NewGCPProvider(opts ...Option) *GCPProvider
    ```

    - **Behavior:** builds a provider that shells out to the `gcloud` CLI.

## status.go

??? note "Signature — `WriteTable`"

    ```go
    func WriteTable(w io.Writer, results []Result)
    ```

    - **Behavior:** renders `Result`s the way `login`/`status`/`logout` show them: one line per provider via `tabwriter`, a `✓`/`✗` mark, and whatever detail `IsAuthenticated` reported.
        - `Err != nil` → `✗`, detail is the error text.
        - not authenticated and no error → `✗`, detail defaults to `"not authenticated — run: kubespin login --only <provider>"` if `Status.Message` is empty.
        - authenticated with `Status.ExpiresAt` set → detail is suffixed with `"(session expires in <duration>)"`.
