# Entrypoints and tooling

This page covers the code under `cmd/` — the binaries kubespin actually
ships — plus the two internal packages that support the build: the docs
generator and the version banner. Each is deliberately thin; the real logic
lives in `internal/`.

## Quick reference

| Component | Role | Summary |
|---|---|---|
| [`cmd/kubespin`](#cmdkubespin) | Operator-facing CLI binary | Wires up a cancellable context and delegates entirely to `internal/cli.NewRootCommand()`. |
| [`cmd/ingestion`](#cmdingestion) | Central Ingestion API Lambda handler | The only inbound network surface in the system; verifies and writes cluster status pushes via `internal/ingestion`. |
| [`cmd/fleet-status-reporter`](#cmdfleet-status-reporter) | In-cluster CronJob binary | Queries local Argo CD, builds a status summary, and pushes it signed to the Central Ingestion API. |
| [`internal/tools/docsgen`](#internaltoolsdocsgen) | `make docs` generator | Regenerates `docs/cli/*.md` from the live cobra command tree so the CLI reference cannot drift. |
| [`internal/version`](#internalversion) | Build metadata package | Carries `Version`/`Commit`/`BuildDate` stamped in via `-ldflags`, and renders the `--version` banner. |

## cmd/kubespin

The operator-facing CLI binary. `main()` in
[cmd/kubespin/main.go](https://github.com/GitOpsHub/kubespin/blob/main/cmd/kubespin/main.go)
does nothing but wire up a cancellable context and delegate to
`internal/cli.NewRootCommand()` — all command definitions, flags, and
business logic live there, not in `cmd/kubespin`. The context is built with
`signal.NotifyContext` against `os.Interrupt` and `syscall.SIGTERM` so that
an interrupted `apply`/`delete` (which can run for tens of minutes) can
release its registry lease instead of leaving a cluster wedged mid-phase.

??? note "Signature: `func main()`"

    ```go
    func main()
    ```

    - **Behavior:** No parameters, no return. Builds the interrupt-cancellable
      context, calls `cli.NewRootCommand().ExecuteContext(ctx)`, and on error
      prints `"kubespin: %v\n"` to stderr and exits with status 1.

## cmd/ingestion

The Central Ingestion API's Lambda handler — the only inbound network
surface in the system, and the endpoint every cluster's
`fleet-status-reporter` pushes signed status to. The handler in
[cmd/ingestion/main.go](https://github.com/GitOpsHub/kubespin/blob/main/cmd/ingestion/main.go)
is a thin adapter over `internal/ingestion.Handler`: it wires up a DynamoDB
Fleet Registry client and a JWKS-backed token verifier, translates between
API Gateway's HTTP v2 event shape and the handler's plain Go signature, and
delegates the actual verification/write logic to `internal/ingestion`. The
package comment notes that the important design point — binding a caller's
OIDC token to the `{clusterId}` in the request path so one cluster's
signature can't be replayed to spoof another — is implemented there, not
in this file.

??? note "Signature: `func newHandler(ctx context.Context) (*ingestion.Handler, error)`"

    ```go
    func newHandler(ctx context.Context) (*ingestion.Handler, error)
    ```

    - **Behavior:** Reads `AWS_REGION` and `REGISTRY_TABLE` from the
      environment, builds a `registry.NewDynamoDB` client and an
      `ingestion.NewVerifier` wrapping `ingestion.NewJWKSResolver(nil)`, and
      returns an `ingestion.NewHandler`. Returns an error if the registry
      connection fails.

??? note "Signature: `func handleRequest(h *ingestion.Handler) func(...)`"

    ```go
    func handleRequest(h *ingestion.Handler) func(context.Context, events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error)
    ```

    - **Behavior:** Returns the Lambda entry function. It pulls `clusterId`
      from the request path parameters and the bearer token from the
      request headers, calls
      `h.HandleStatus(ctx, clusterID, token, []byte(req.Body))`, and
      marshals the response as JSON. If marshalling that response fails
      (which the comment notes "cannot realistically fail" since the
      response is a plain struct of strings and a bool), it degrades to a
      hardcoded `{"error":"internal_error"}` body with a 500 status rather
      than surfacing a marshal error to the caller. Always returns `nil`
      for the error value — failures are encoded in the HTTP status/body,
      not the Go error.

??? note "Signature: `func bearerToken(headers map[string]string) string`"

    ```go
    func bearerToken(headers map[string]string) string
    ```

    - **Behavior:** Case-insensitively finds the `Authorization` header
      (API Gateway may lower-case header keys) and strips a `"Bearer "`
      prefix. Returns `""` if no matching header or prefix is found.

??? note "Signature: `func main()`"

    ```go
    func main()
    ```

    - **Behavior:** Sets up a JSON `slog` logger to stderr (structured
      fields are queryable in CloudWatch Logs, unlike free text), builds
      the handler via `newHandler`, exits 1 and logs an error if that
      fails, logs a startup message with the region and table, and calls
      `lambda.Start(handleRequest(h))`.

## cmd/fleet-status-reporter

The in-cluster CronJob binary. Per the package comment in
[cmd/fleet-status-reporter/main.go](https://github.com/GitOpsHub/kubespin/blob/main/cmd/fleet-status-reporter/main.go),
it queries the cluster's local Argo CD instance, builds a compact status
summary, and pushes it to the Central Ingestion API, signed with the
cluster's workload identity token. It is deliberately a single push per
invocation rather than a long-running loop — the Kubernetes CronJob
resource owns the schedule, so this binary's whole job is to run once,
push once, and report success or failure through its exit code. All of
the argo-cd-querying and push/signing logic lives in `internal/reporter`;
this file only reads environment configuration and wires it together.

Required environment variables (checked as a group; if any is empty, `run`
returns `errRequiredEnv`):

- `CLUSTER_ID`
- `ARGOCD_SERVER`
- `INGESTION_URL`

Optional:

- `ARGOCD_TOKEN`
- `IDENTITY_TOKEN_PATH` (defaults to `/var/run/secrets/kubespin/token`)

??? note "Signature: `func main()`"

    ```go
    func main()
    ```

    - **Behavior:** Builds a JSON `slog` logger to stderr (its stderr is
      scraped into the cluster's log pipeline), calls `run(logger)`, and on
      error logs it and exits 1.

??? note "Signature: `func run(logger *slog.Logger) error`"

    ```go
    func run(logger *slog.Logger) error
    ```

    - **Behavior:** Reads and validates the required env vars (returning
      `errRequiredEnv` if any are missing), resolves the token path (env
      var or `defaultTokenPath`), builds a `context.WithTimeout` of
      `defaultPushDeadline` (30 seconds), constructs a
      `reporter.NewHTTPArgoCDClient` and a `reporter.NewPusher` (using
      `reporter.FileTokenSource{Path: tokenPath}` as the token source), and
      calls `pusher.Push(ctx, argocd)`. Returns a wrapped error if the push
      itself errors, `errRejected` if the Central Ingestion API did not
      accept the push (`accepted == false`), and `nil` on success (also
      logging an "accepted" message).

## internal/tools/docsgen

Backs `make docs`. Regenerates `docs/cli/*.md` from the live cobra command
tree defined in `internal/cli`, so the CLI reference cannot drift from the
actual flags/commands — per the package comment in
[internal/tools/docsgen/main.go](https://github.com/GitOpsHub/kubespin/blob/main/internal/tools/docsgen/main.go),
CI regenerates it and fails if the result differs from what's committed.

??? note "Signature: `func main()`"

    ```go
    func main()
    ```

    - **Behavior:** Calls `run()`; on error prints `"docsgen: %v\n"` to
      stderr and exits 1.

??? note "Signature: `func run() error`"

    ```go
    func run() error
    ```

    - **Behavior:** Creates `docs/cli` (`outputDir`) if needed, builds
      `cli.NewRootCommand()`, calls `disableAutoGenTag` on it (so cobra
      doesn't stamp the current date into generated files, which would make
      every rebuild look dirty), clears `root.Version` (so the build commit
      embedded in the version string doesn't churn the root page on every
      rebuild), runs `doc.GenMarkdownTree(root, outputDir)` (cobra's own
      doc generator), then calls `polishAll(outputDir)`.

??? note "Signature: `func polishAll(dir string) error`"

    ```go
    func polishAll(dir string) error
    ```

    - **Behavior:** Globs `dir/*.md`, and for each file rewrites its
      content via `polish` and writes it back.

??? note "Signature: `func polish(md string) string`"

    ```go
    func polish(md string) string
    ```

    - **Behavior:** Rewrites cobra's generated markdown into the shape the
      rest of the hand-written docs use, per line, tracking whether it is
      currently inside a fenced code block (so headings inside shell
      comments aren't rewritten):
        - Cobra's page title is emitted as `##`; promoted to `#` since
          MkDocs derives the page title and TOC from the H1.
        - `### ` section headings are demoted to `##`, and `SEE ALSO` is
          rewritten to sentence case `See also`.
        - Code fences (` ``` `) get a language tag from `fenceLanguage`,
          since cobra emits untagged fences and nothing gets
          syntax-highlighted otherwise.
        - "See also" list entries (`* [`) have tab characters stripped,
          since a tab between the link and its description renders as a
          ragged gap.

??? note "Signature: `func fenceLanguage(section string) string`"

    ```go
    func fenceLanguage(section string) string
    ```

    - **Behavior:** Returns `"bash"` if the current section is
      `"Examples"`, else `"text"` — only the examples are shell; usage
      synopsis and option lists are output shapes that would be
      mis-highlighted as shell (e.g. `[flags]`).

??? note "Signature: `func disableAutoGenTag(cmd *cobra.Command)`"

    ```go
    func disableAutoGenTag(cmd *cobra.Command)
    ```

    - **Behavior:** Recursively sets `cmd.DisableAutoGenTag = true` on the
      command and all of its children.

## internal/version

A single-file package carrying build metadata stamped in via `-ldflags` at
build time (see the Makefile). Per the package comment in
[internal/version/version.go](https://github.com/GitOpsHub/kubespin/blob/main/internal/version/version.go),
defaults keep `go run` usable without a full `make build`.

??? abstract "Signature: `var (Version, Commit, BuildDate string)`"

    ```go
    var (
        Version   = "dev"
        Commit    = "unknown"
        BuildDate = "unknown"
    )
    ```

    - **Behavior:** Package-level variables, overridden at build time via
      linker flags.

??? note "Signature: `func String() string`"

    ```go
    func String() string
    ```

    - **Behavior:** Renders the version banner shown by
      `kubespin --version`:
      `"kubespin %s (commit %s, built %s, %s/%s, %s)"`, formatted with
      `Version`, `Commit`, `BuildDate`, `runtime.GOOS`, `runtime.GOARCH`,
      and `runtime.Version()`.
