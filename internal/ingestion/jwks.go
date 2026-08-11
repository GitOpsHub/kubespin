package ingestion

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// JWKSResolver resolves signing keys the standard OIDC way: fetch the
// issuer's discovery document, fetch its JWKS, find the key matching kid.
//
// This is the one part of this package that reaches the network — to each
// cluster's own cloud-hosted OIDC issuer, not to anything under kubespin's
// control. Its caching and rotation behaviour is exercised against an
// httptest issuer (jwks_test.go), since a stale cache is a silent
// per-cluster outage rather than a visible failure. Verifier's own tests
// supply a fixed key through KeyResolver instead, which is enough to prove
// the verification logic (issuer binding, expiry, signature) is correct.
type JWKSResolver struct {
	client *http.Client
	now    func() time.Time

	// mu guards cache. The Lambda runtime delivers one invocation at a time
	// per execution environment, so this is not contended in production today
	// — but an unsynchronised map is a fatal "concurrent map writes" panic the
	// first time this resolver is served from anything concurrent, and it is
	// an exported type.
	mu    sync.Mutex
	cache map[string]keySet
}

// keySet is one issuer's cached JWKS and when it was fetched.
type keySet struct {
	keys      []jwk
	fetchedAt time.Time
}

const (
	// jwksCacheTTL bounds how long a key set is trusted without a refetch.
	// Caching avoids a discovery + JWKS round trip on every status push, and
	// every cluster pushes every couple of minutes.
	jwksCacheTTL = time.Hour

	// jwksMinRefetchInterval rate-limits the refetch an unknown kid triggers,
	// so a stream of tokens bearing garbage kids cannot turn into a stream of
	// outbound requests to the issuer.
	jwksMinRefetchInterval = time.Minute
)

// NewJWKSResolver builds a resolver using client, or http.DefaultClient if
// client is nil.
func NewJWKSResolver(client *http.Client) *JWKSResolver {
	if client == nil {
		client = http.DefaultClient
	}
	return &JWKSResolver{client: client, now: time.Now, cache: map[string]keySet{}}
}

type oidcDiscovery struct {
	JWKSURI string `json:"jwks_uri"`
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	// RSA
	N string `json:"n"`
	E string `json:"e"`
	// EC
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// Resolve implements KeyResolver.
//
// A cache miss on kid triggers a refetch rather than an immediate rejection.
// That is what makes this survive issuer key rotation: every cloud rotates
// its clusters' OIDC signing keys, and a cached set that is never revisited
// starts failing every push from that cluster the moment it does — a silent,
// per-cluster outage that looks like a bad token and lasts until the process
// happens to restart. Since the whole architecture depends on clusters
// pushing status outward, a cluster that cannot push is a cluster the fleet
// stops being able to see.
func (r *JWKSResolver) Resolve(ctx context.Context, issuer, kid string) (any, error) {
	if k, ok := r.cachedKey(issuer, kid); ok {
		return publicKeyFromJWK(k)
	}

	keys, err := r.refresh(ctx, issuer)
	if err != nil {
		return nil, err
	}
	if k, ok := findKey(keys, kid); ok {
		return publicKeyFromJWK(k)
	}
	return nil, fmt.Errorf("no signing key %q found for issuer %s", kid, issuer)
}

// cachedKey returns kid from the cached set for issuer, if the set is still
// fresh and actually contains it.
func (r *JWKSResolver) cachedKey(issuer, kid string) (jwk, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.cache[issuer]
	if !ok || r.now().Sub(entry.fetchedAt) >= jwksCacheTTL {
		return jwk{}, false
	}
	return findKey(entry.keys, kid)
}

// refresh fetches issuer's keys, subject to the refetch rate limit. When the
// limit applies it returns whatever is cached, so the caller reports an
// unknown key rather than hammering the issuer.
func (r *JWKSResolver) refresh(ctx context.Context, issuer string) ([]jwk, error) {
	r.mu.Lock()
	entry, cached := r.cache[issuer]
	if cached && r.now().Sub(entry.fetchedAt) < jwksMinRefetchInterval {
		r.mu.Unlock()
		return entry.keys, nil
	}
	r.mu.Unlock()

	// Fetched outside the lock: this is a network round trip, and holding a
	// mutex across it would serialise every verification behind the slowest
	// issuer.
	keys, err := r.fetchKeys(ctx, issuer)
	if err != nil {
		// A failed refetch must not discard a cached set that may still be
		// perfectly valid — the issuer being briefly unreachable is not
		// evidence its keys changed.
		if cached {
			return entry.keys, nil
		}
		return nil, err
	}

	r.mu.Lock()
	r.cache[issuer] = keySet{keys: keys, fetchedAt: r.now()}
	r.mu.Unlock()

	return keys, nil
}

func findKey(keys []jwk, kid string) (jwk, bool) {
	for _, k := range keys {
		if k.Kid == kid {
			return k, true
		}
	}
	return jwk{}, false
}

func (r *JWKSResolver) fetchKeys(ctx context.Context, issuer string) ([]jwk, error) {
	var discovery oidcDiscovery
	if err := r.getJSON(ctx, issuer+"/.well-known/openid-configuration", &discovery); err != nil {
		return nil, fmt.Errorf("fetching OIDC discovery for %s: %w", issuer, err)
	}
	if discovery.JWKSURI == "" {
		return nil, fmt.Errorf("OIDC discovery for %s has no jwks_uri", issuer)
	}

	var set jwkSet
	if err := r.getJSON(ctx, discovery.JWKSURI, &set); err != nil {
		return nil, fmt.Errorf("fetching JWKS for %s: %w", issuer, err)
	}
	return set.Keys, nil
}

func (r *JWKSResolver) getJSON(ctx context.Context, url string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building request for %s: %w", url, err)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("requesting %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("requesting %s: unexpected status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response from %s: %w", url, err)
	}
	return nil
}

// publicKeyFromJWK builds a Go crypto public key from a JWK's RSA or EC
// fields — the two key types every cloud's OIDC issuer uses.
func publicKeyFromJWK(k jwk) (any, error) {
	switch k.Kty {
	case "RSA":
		n, err := base64URLBigInt(k.N)
		if err != nil {
			return nil, fmt.Errorf("decoding RSA modulus: %w", err)
		}
		e, err := base64URLBigInt(k.E)
		if err != nil {
			return nil, fmt.Errorf("decoding RSA exponent: %w", err)
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil

	case "EC":
		curve, err := ecCurve(k.Crv)
		if err != nil {
			return nil, err
		}
		x, err := base64URLBigInt(k.X)
		if err != nil {
			return nil, fmt.Errorf("decoding EC x: %w", err)
		}
		y, err := base64URLBigInt(k.Y)
		if err != nil {
			return nil, fmt.Errorf("decoding EC y: %w", err)
		}
		return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil

	default:
		return nil, fmt.Errorf("unsupported key type %q", k.Kty)
	}
}

func ecCurve(name string) (elliptic.Curve, error) {
	switch name {
	case "P-256":
		return elliptic.P256(), nil
	case "P-384":
		return elliptic.P384(), nil
	case "P-521":
		return elliptic.P521(), nil
	default:
		return nil, fmt.Errorf("unsupported EC curve %q", name)
	}
}

func base64URLBigInt(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decoding base64url value: %w", err)
	}
	return new(big.Int).SetBytes(b), nil
}
