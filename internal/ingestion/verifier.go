// Package ingestion implements the Central Ingestion API's verification and
// write path: the only inbound surface in the whole system, per this
// project's outbound-only architecture invariant.
package ingestion

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// KeyResolver resolves the public signing key for a token's issuer and key
// ID. The production implementation (JWKSResolver) fetches it from the
// issuer's JWKS via OIDC discovery over the network; tests supply a fixed
// key, which is what lets the token-verification logic below — the anti-
// replay property this package exists for — be exercised by a real signed
// JWT in a unit test rather than only mocked away.
type KeyResolver interface {
	Resolve(ctx context.Context, issuer, kid string) (any, error)
}

// Claims is what a verified fleet-status-reporter token proves.
type Claims struct {
	Issuer    string
	Subject   string
	Audience  []string
	ExpiresAt time.Time
}

// ErrTokenInvalid wraps every reason a token fails verification, so callers
// can distinguish "the caller isn't who they claim to be" from an
// infrastructure error (registry unreachable, JWKS fetch failed) without
// string-matching.
var ErrTokenInvalid = errors.New("token failed verification")

// Verifier verifies a fleet-status-reporter token was issued by
// expectedIssuer — the OIDC issuer recorded for the specific cluster the
// caller claims to be reporting status for.
//
// Binding verification to that one specific, per-cluster issuer (rather than
// accepting any token any trusted key resolver would validate) is the whole
// anti-replay property M6 needs: every cluster's workload identity issuer is
// unique (AWS/GCP/Azure all mint one per cluster), so a token that verifies
// against cluster A's issuer cannot also verify against cluster B's — there
// is no shared trust root for an attacker to lean on.
type Verifier struct {
	keys KeyResolver
}

// NewVerifier builds a Verifier over keys.
func NewVerifier(keys KeyResolver) *Verifier { return &Verifier{keys: keys} }

// Verify checks tokenString's signature, expiry, and issuer against
// expectedIssuer, returning its claims.
func (v *Verifier) Verify(ctx context.Context, tokenString, expectedIssuer string) (Claims, error) {
	if expectedIssuer == "" {
		return Claims{}, fmt.Errorf("%w: no issuer is on record for this cluster", ErrTokenInvalid)
	}

	var claims jwt.RegisteredClaims
	token, err := jwt.ParseWithClaims(tokenString, &claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.keys.Resolve(ctx, expectedIssuer, kid)
	}, jwt.WithValidMethods([]string{"RS256", "ES256"}), jwt.WithIssuer(expectedIssuer), jwt.WithExpirationRequired())
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %w", ErrTokenInvalid, err)
	}
	if !token.Valid {
		return Claims{}, fmt.Errorf("%w: token is not valid", ErrTokenInvalid)
	}

	exp, err := claims.GetExpirationTime()
	if err != nil || exp == nil {
		return Claims{}, fmt.Errorf("%w: missing expiry", ErrTokenInvalid)
	}

	return Claims{
		Issuer: claims.Issuer, Subject: claims.Subject,
		Audience: []string(claims.Audience), ExpiresAt: exp.Time,
	}, nil
}
