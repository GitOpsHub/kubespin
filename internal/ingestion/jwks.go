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
	"time"
)

// JWKSResolver resolves signing keys the standard OIDC way: fetch the
// issuer's discovery document, fetch its JWKS, find the key matching kid.
//
// This is the one part of this package that reaches the network — to each
// cluster's own cloud-hosted OIDC issuer, not to anything under kubespin's
// control — and so is the one part not exercised by this package's tests,
// the same way this project's cloud provisioners' tests stop at the SDK
// boundary. Verifier's tests instead supply a fixed key through KeyResolver,
// which is enough to prove the verification logic itself (issuer binding,
// expiry, signature) is correct.
type JWKSResolver struct {
	client *http.Client
	// cache avoids a discovery + JWKS round trip per status push; entries
	// never expire within a process lifetime because a cluster's issuer keys
	// only rotate on a timescale far longer than any single process runs.
	cache map[string][]jwk
}

// NewJWKSResolver builds a resolver using client, or http.DefaultClient if
// client is nil.
func NewJWKSResolver(client *http.Client) *JWKSResolver {
	if client == nil {
		client = http.DefaultClient
	}
	return &JWKSResolver{client: client, cache: map[string][]jwk{}}
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
func (r *JWKSResolver) Resolve(ctx context.Context, issuer, kid string) (any, error) {
	keys, ok := r.cache[issuer]
	if !ok {
		var err error
		keys, err = r.fetchKeys(ctx, issuer)
		if err != nil {
			return nil, err
		}
		r.cache[issuer] = keys
	}

	for _, k := range keys {
		if k.Kid != kid {
			continue
		}
		return publicKeyFromJWK(k)
	}
	return nil, fmt.Errorf("no signing key %q found for issuer %s", kid, issuer)
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
