# internal/reporter

`internal/reporter` implements the two halves of fleet-status-reporter's
work: querying the cluster's local Argo CD instance for a compact application
summary (`argocd.go`), and pushing that summary to the Central Ingestion API
signed with the cluster's workload identity token (`pusher.go`). The client
calls Argo CD's `GET /api/v1/applications` over the local, in-namespace
network and reduces the response to synced/healthy/degraded counts plus the
last synced commit SHA — never the full application list. The pusher then
reads a projected Kubernetes service-account token (the cluster's own OIDC
issuer signed it when the kubelet projected it), attaches it as a Bearer
token, and `POST`s the summary as JSON to
`{endpoint}/clusters/{clusterID}/status` on the Central Ingestion API — the
only outbound connection this whole architecture allows a cluster to make
about itself.

## Types

### `Summary`

The compact status extracted from Argo CD: counts, not the full application
list, because the Central Ingestion API and Fleet Registry only need to know
"is this cluster healthy."

```go
type Summary struct {
	SyncedApps   int
	HealthyApps  int
	DegradedApps int
	CommitSHA    string
}
```

- `CommitSHA` is set from the first synced application's revision seen while
  iterating (see `summarize`, `argocd.go`).
- `DegradedApps` counts only Argo CD's `Degraded` health status — deliberately
  narrow. Argo CD also reports `Progressing`, `Missing`, `Unknown`, which are
  transient/informational and are not folded in, so fleet status doesn't get
  noisy on every routine rollout.

### `ArgoCDClient`

Interface a `Pusher` depends on to summarize the local Argo CD instance's
application state.

```go
type ArgoCDClient interface {
	Summarize(ctx context.Context) (Summary, error)
}
```

### `HTTPArgoCDClient`

Calls the local Argo CD server's REST API over the in-cluster network — the
one connection inbound-to-the-namespace (fleet-status-reporter to Argo CD)
that this architecture allows, as distinct from inbound-to-the-cluster from
outside, which it never allows.

```go
type HTTPArgoCDClient struct {
	// unexported: client *http.Client, baseURL, token string, logger *slog.Logger
}
```

- Implements `ArgoCDClient`.
- Constructed via `NewHTTPArgoCDClient`.
- Authenticates to Argo CD with its own API token (`token`) — distinct from
  the workload identity token `Pusher` later signs the ingestion push with;
  the two prove different things to different services.

### `TokenSource`

Interface for reading the workload identity token `Pusher` signs its push
with. Nothing in this package mints or signs a token itself.

```go
type TokenSource interface {
	Token() (string, error)
}
```

### `FileTokenSource`

Reads a token from a projected volume file — the standard Kubernetes pattern
for an audience-scoped service account token (`serviceAccountToken` volume
projection).

```go
type FileTokenSource struct {
	Path string
}
```

- Implements `TokenSource`.
- `Token()` reads `Path`, trims surrounding whitespace, and returns the
  contents as a string.

### `Pusher`

Pushes one cluster's status to the Central Ingestion API.

```go
type Pusher struct {
	// unexported: client *http.Client, endpoint string, clusterID core.ClusterID,
	// tokens TokenSource, logger *slog.Logger
}
```

- Constructed via `NewPusher`.
- `Push` is the only method (see below).

### `Option` / `WithLogger`

Shared functional-option plumbing for this package's constructors.

```go
type Option func(*options)

func WithLogger(logger *slog.Logger) Option
```

- `WithLogger` sets the logger a component uses; without it, a component logs
  to `slog.Default()`. A nil `logger` argument is ignored rather than clearing
  the default.

## Functions

### `NewHTTPArgoCDClient`

```go
func NewHTTPArgoCDClient(client *http.Client, baseURL, token string, opts ...Option) *HTTPArgoCDClient
```

- **Params:** `client` (nil defaults to `http.DefaultClient`), `baseURL`
  (typically `"https://argocd-server.argocd.svc:443"`), `token` (Argo CD's
  own API token), `opts` (e.g. `WithLogger`).
- **Returns:** a configured `*HTTPArgoCDClient`.
- **Behavior:** pure construction, no I/O.

### `(*HTTPArgoCDClient) Summarize`

```go
func (c *HTTPArgoCDClient) Summarize(ctx context.Context) (Summary, error)
```

- **Params:** `ctx` for request cancellation/timeout.
- **Returns:** a `Summary` built from Argo CD's application list, or an error
  wrapping a request-building, network, non-200-status, or JSON-decode
  failure.
- **Behavior:** `GET`s `{baseURL}/api/v1/applications` (Bearer-authenticated
  if `token` is non-empty), decodes the response into an internal
  `applicationList`/`application` shape (mirroring only `status.sync.status`,
  `status.sync.revision`, and `status.health.status`), and reduces it via
  `summarize`. Logs the query, the resulting counts, and a warning if any
  application is degraded.

### `NewPusher`

```go
func NewPusher(
	client *http.Client, endpoint string, clusterID core.ClusterID, tokens TokenSource, opts ...Option,
) *Pusher
```

- **Params:** `client` (nil defaults to `http.DefaultClient`), `endpoint`
  (Central Ingestion API base URL, e.g.
  `"https://ingest.kubespin.example.com"`), `clusterID`, `tokens`, `opts`.
- **Returns:** a configured `*Pusher`.
- **Behavior:** pure construction, no I/O.

### `(*Pusher) Push`

```go
func (p *Pusher) Push(ctx context.Context, argocd ArgoCDClient) (accepted bool, err error)
```

- **Params:** `ctx`, and `argocd` — the `ArgoCDClient` to summarize before
  pushing.
- **Returns:** `accepted` reports whether the ingestion API's HTTP status was
  2xx; `err` is non-nil only for a failure in this method's own steps
  (summarizing Argo CD, reading the token, encoding the payload, building or
  sending the HTTP request). A non-2xx response (bad token, unknown cluster)
  is **not** a Go error — the doc comment is explicit that this is a normal,
  expected outcome the caller's exit code decides how to react to, mirroring
  how `ingestion.HandleStatus` on the receiving end returns a status code
  rather than only an error.
- **Behavior:**
  1. Calls `argocd.Summarize(ctx)`.
  2. Calls `p.tokens.Token()` to get the workload identity token.
  3. Marshals an `ingestion.StatusPayload{SyncedApps, HealthyApps, DegradedApps, CommitSHA}` from the summary.
  4. `POST`s the JSON payload to `{endpoint}/clusters/{clusterID}/status`
     with `Authorization: Bearer {token}` and `Content-Type: application/json`.
  5. Logs acceptance (info) or rejection (warn), including the response
     status code.

## Wiring

`cmd/fleet-status-reporter/main.go` reads `CLUSTER_ID`, `ARGOCD_SERVER`,
`ARGOCD_TOKEN`, `INGESTION_URL`, and `IDENTITY_TOKEN_PATH` from the
environment, then builds a `reporter.NewHTTPArgoCDClient` and
`reporter.NewPusher` (with `reporter.FileTokenSource`) and calls `Push` once
under a 30-second timeout, translating the result into the CronJob's process
exit code.
