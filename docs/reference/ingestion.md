# internal/ingestion

`internal/ingestion` implements the Central Ingestion API's verification and write path — the only inbound surface in the whole system, per the outbound-only architecture invariant. It verifies that a status push's bearer token is a valid, unexpired JWT signed by the OIDC issuer recorded in the Fleet Registry for the specific `ClusterID` named in the request, which is what stops a signature legitimately issued to cluster A from being replayed to spoof cluster B: every cloud mints a unique workload-identity issuer per cluster, so there is no shared trust root a forged or replayed token from another cluster could lean on. Subject and audience checks then narrow trust further, rejecting any other in-cluster workload that happens to hold a token from that same issuer. Once verification passes, the handler records the push by calling `registry.Registry.Touch`, updating the cluster's last-seen state in DynamoDB — the write path is a single conditional-free `Touch`, not a full record rewrite.

## Types

### `KeyResolver`

*(verifier.go)*

```go
type KeyResolver interface {
	Resolve(ctx context.Context, issuer, kid string) (any, error)
}
```

Resolves the public signing key for a token's issuer and key ID. The production implementation is `JWKSResolver`, which fetches the key from the issuer's JWKS via OIDC discovery over the network. Tests supply a fixed key instead, so the anti-replay verification logic can be exercised against a real signed JWT without network access.

### `Claims`

*(verifier.go)*

```go
type Claims struct {
	Issuer    string
	Subject   string
	Audience  []string
	ExpiresAt time.Time
}
```

What a verified fleet-status-reporter token proves: which issuer signed it, which service account it names, which audiences it was minted for, and when it expires.

### `Verifier`

*(verifier.go)*

```go
type Verifier struct {
	keys   KeyResolver
	logger *slog.Logger
}
```

Verifies a fleet-status-reporter token was issued by one specific, per-cluster `expectedIssuer` — the OIDC issuer recorded for the cluster the caller claims to represent — rather than accepting any token any trusted key resolver would validate. That binding is the anti-replay property this package exists for: a token that verifies against cluster A's issuer cannot also verify against cluster B's.

**Method:**

```go
func (v *Verifier) Verify(ctx context.Context, tokenString, expectedIssuer string) (Claims, error)
```

Checks `tokenString`'s signature, expiry, and issuer against `expectedIssuer`, returning its `Claims`. Uses `jwt.ParseWithClaims` restricted to `RS256`/`ES256`, requires the issuer to match `expectedIssuer` exactly (`jwt.WithIssuer`), and requires an expiration claim to be present (`jwt.WithExpirationRequired`). Returns an error wrapping `ErrTokenInvalid` when `expectedIssuer` is empty (no issuer on record for the cluster), when parsing/signature/issuer checks fail, or when the expiry claim is missing.

**Invariants:**
- An empty `expectedIssuer` is always rejected — there is no "trust any issuer" fallback.
- Only `RS256` and `ES256` are accepted signing algorithms.
- Every failure is wrapped in `ErrTokenInvalid` so callers can distinguish "the caller isn't who it claims to be" from an infrastructure error (registry unreachable, JWKS fetch failed) without string-matching.

### `ErrTokenInvalid`

*(verifier.go)*

```go
var ErrTokenInvalid = errors.New("token failed verification")
```

Sentinel wrapping every reason a token fails verification.

### `JWKSResolver`

*(jwks.go)*

```go
type JWKSResolver struct {
	client *http.Client
	now    func() time.Time
	mu     sync.Mutex
	cache  map[string]keySet
}
```

Implements `KeyResolver` the standard OIDC way: fetch the issuer's `/.well-known/openid-configuration` discovery document, fetch its `jwks_uri`, and find the key matching the token's `kid`. This is the one part of the package that reaches the network — to each cluster's own cloud-hosted OIDC issuer, never to anything under kubespin's control.

**Method:**

```go
func (r *JWKSResolver) Resolve(ctx context.Context, issuer, kid string) (any, error)
```

Returns the cached key for `(issuer, kid)` if the cache entry is fresh (`jwksCacheTTL`) and contains `kid`; otherwise triggers a refetch (subject to `jwksMinRefetchInterval`) and looks again. A cache miss on `kid` triggers a refetch rather than immediate rejection so key rotation doesn't strand a cluster with a stale cached set.

**Invariants:**
- `jwksCacheTTL` (1 hour) bounds how long a key set is trusted without a refetch.
- `jwksMinRefetchInterval` (1 minute) rate-limits refetches triggered by unknown `kid`s, so a stream of tokens with garbage `kid`s cannot become a stream of outbound requests to the issuer.
- A failed refetch does not discard a still-cached key set — an issuer being briefly unreachable is not evidence its keys changed; only a successful fetch replaces the cache.
- Keys are fetched outside the internal mutex so a slow issuer does not serialize verification for every other issuer.
- Supports RSA and EC (`P-256`, `P-384`, `P-521`) JWKs, the two key types every cloud's OIDC issuer uses.

**Constructor:**

```go
func NewJWKSResolver(client *http.Client) *JWKSResolver
```

Builds a resolver using `client`, or `http.DefaultClient` if `client` is nil.

### `StatusPayload`

*(handler.go)*

```go
type StatusPayload struct {
	SyncedApps   int    `json:"syncedApps"`
	HealthyApps  int    `json:"healthyApps"`
	DegradedApps int    `json:"degradedApps"`
	CommitSHA    string `json:"commitSha"`
}
```

The compact status fleet-status-reporter pushes: a summary of what its local Argo CD reports, not the full application list.

### `Response`

*(handler.go)*

```go
type Response struct {
	Accepted  bool   `json:"accepted"`
	ClusterID string `json:"clusterId,omitempty"`
	Error     string `json:"error,omitempty"`
	Message   string `json:"message,omitempty"`
}
```

`HandleStatus`'s result body, returned on both success and failure, with a machine-readable `Error` code distinguishing failure modes (`missing_token`, `unknown_cluster`, `registry_error`, `invalid_token`, `wrong_subject`, `wrong_audience`, `invalid_body`).

### `Handler`

*(handler.go)*

```go
type Handler struct {
	reg      registry.Registry
	verifier *Verifier
	now      func() time.Time
	logger   *slog.Logger
}
```

Implements the Central Ingestion API's one operation: accept a signed status push from a cluster's fleet-status-reporter, verify it, and record it in the Fleet Registry.

**Method:**

```go
func (h *Handler) HandleStatus(ctx context.Context, clusterID core.ClusterID, token string, body []byte) (int, Response)
```

Verifies `token` proves the caller is `clusterID`'s fleet-status-reporter, then records the push. Steps, each returning a distinct HTTP status and `Error` code rather than a bare 500:

1. `token == ""` → `401 missing_token`.
2. `h.reg.Get(ctx, clusterID)` — `registry.ErrNotFound` → `404 unknown_cluster`; any other error → `500 registry_error`.
3. `h.verifier.Verify(ctx, token, rec.OIDCIssuer)` fails → `403 invalid_token`. This is the per-cluster issuer binding that prevents cross-cluster replay.
4. `claims.Subject != ExpectedSubject` → `403 wrong_subject`.
5. `!slices.Contains(claims.Audience, ExpectedAudience)` → `403 wrong_audience`.
6. Body, if non-empty, must unmarshal into `StatusPayload` → otherwise `400 invalid_body`.
7. `h.reg.Touch(ctx, clusterID, h.now())` fails → `500 registry_error`. On success → `202` with `Response{Accepted: true, ClusterID: clusterID.String()}`.

**Invariants:**
- Issuer binding (step 3) and subject/audience checks (steps 4–5) are independent: issuer binding stops cross-cluster replay; subject/audience stop a different in-cluster workload that happens to hold a token from the *same* issuer.
- The registry write is a single `Touch` call — the handler never mutates any other registry field.

**Constructor:**

```go
func NewHandler(reg registry.Registry, verifier *Verifier, opts ...Option) *Handler
```

Builds a `Handler` over `reg` and `verifier`; `now` defaults to `time.Now`.

### `Option` / `WithLogger`

*(verifier.go)*

```go
type Option func(*options)
func WithLogger(logger *slog.Logger) Option
```

Configures the logger used by `Verifier` and `Handler`. Without it, a component logs to `slog.Default()`.

## Constants

```go
const ExpectedSubject = "system:serviceaccount:kubespin-system:fleet-status-reporter"
const ExpectedAudience = "kubespin-ingestion"
```

*(handler.go)* — `ExpectedSubject` is the Kubernetes service-account subject every fleet-status-reporter token must present, matching the provisioner's `StatusReporter()` component every `IdentityProvisioner` binds workload identity to. `ExpectedAudience` scopes the token to this API and nothing else.

## Wiring

`cmd/ingestion/main.go` builds the Lambda entry point by constructing `registry.NewDynamoDB`, `ingestion.NewJWKSResolver(nil)`, `ingestion.NewVerifier`, and `ingestion.NewHandler`, then adapts `Handler.HandleStatus` to an `events.APIGatewayV2HTTPRequest`/`Response` pair (extracting `clusterId` from the path and the bearer token from the `Authorization` header) before registering it with `lambda.Start`.
