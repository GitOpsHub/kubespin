package ingestion

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// fakeKeyResolver stands in for a real JWKS fetch: it returns a fixed key
// regardless of issuer/kid, which is enough to exercise Verify's actual
// signature-checking logic against a genuinely signed token rather than
// mocking verification away entirely.
type fakeKeyResolver struct {
	key any
	err error
}

func (f fakeKeyResolver) Resolve(context.Context, string, string) (any, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.key, nil
}

// signedToken builds and signs a real JWT with claims, for tests to feed
// through the real jwt.ParseWithClaims path Verify uses.
func signedToken(t *testing.T, key *rsa.PrivateKey, claims jwt.RegisteredClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "test-key"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("signing test token: %v", err)
	}
	return signed
}

const testIssuer = "https://oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"

func TestVerifier_Verify_ValidToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}

	token := signedToken(t, key, jwt.RegisteredClaims{
		Issuer:    testIssuer,
		Subject:   ExpectedSubject,
		Audience:  jwt.ClaimStrings{ExpectedAudience},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})

	v := NewVerifier(fakeKeyResolver{key: &key.PublicKey})
	claims, err := v.Verify(context.Background(), token, testIssuer)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != ExpectedSubject {
		t.Errorf("Subject = %q, want %q", claims.Subject, ExpectedSubject)
	}
	if claims.Issuer != testIssuer {
		t.Errorf("Issuer = %q, want %q", claims.Issuer, testIssuer)
	}
}

func TestVerifier_Verify_WrongIssuerRejected(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}

	// Signed for cluster A's issuer...
	token := signedToken(t, key, jwt.RegisteredClaims{
		Issuer: testIssuer, Subject: ExpectedSubject,
		Audience: jwt.ClaimStrings{ExpectedAudience}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})

	// ...must not verify against cluster B's issuer. This is the anti-replay
	// property this whole package exists to enforce.
	v := NewVerifier(fakeKeyResolver{key: &key.PublicKey})
	if _, err := v.Verify(context.Background(), token, "https://a-different-clusters-issuer.example.com"); err == nil {
		t.Fatal("expected a token signed for a different issuer to be rejected")
	}
}

func TestVerifier_Verify_ExpiredTokenRejected(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}

	token := signedToken(t, key, jwt.RegisteredClaims{
		Issuer: testIssuer, Subject: ExpectedSubject,
		Audience: jwt.ClaimStrings{ExpectedAudience}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
	})

	v := NewVerifier(fakeKeyResolver{key: &key.PublicKey})
	if _, err := v.Verify(context.Background(), token, testIssuer); err == nil {
		t.Fatal("expected an expired token to be rejected")
	}
}

func TestVerifier_Verify_MissingExpiryRejected(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}

	token := signedToken(t, key, jwt.RegisteredClaims{
		Issuer: testIssuer, Subject: ExpectedSubject, Audience: jwt.ClaimStrings{ExpectedAudience},
	})

	v := NewVerifier(fakeKeyResolver{key: &key.PublicKey})
	if _, err := v.Verify(context.Background(), token, testIssuer); err == nil {
		t.Fatal("expected a token with no expiry to be rejected")
	}
}

func TestVerifier_Verify_WrongSigningKeyRejected(t *testing.T) {
	signingKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating signing key: %v", err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating other key: %v", err)
	}

	token := signedToken(t, signingKey, jwt.RegisteredClaims{
		Issuer: testIssuer, Subject: ExpectedSubject,
		Audience: jwt.ClaimStrings{ExpectedAudience}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})

	// The resolver returns a key that did not sign this token — simulating a
	// forged token presented against the real issuer's genuine JWKS.
	v := NewVerifier(fakeKeyResolver{key: &otherKey.PublicKey})
	if _, err := v.Verify(context.Background(), token, testIssuer); err == nil {
		t.Fatal("expected a token signed by an untrusted key to be rejected")
	}
}

func TestVerifier_Verify_NoIssuerOnRecord(t *testing.T) {
	v := NewVerifier(fakeKeyResolver{})
	if _, err := v.Verify(context.Background(), "irrelevant", ""); err == nil {
		t.Fatal("expected verification to fail when no issuer is recorded for the cluster")
	}
}
