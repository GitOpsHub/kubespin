package ingestion

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/registry"
)

func seedClusterWithIssuer(t *testing.T, reg registry.Registry, id core.ClusterID, issuer string) {
	t.Helper()
	spec := core.ClusterSpec{
		ID: id, Provider: core.ProviderAWS, Region: "us-east-1", Access: core.AccessPrivate,
		Profile: core.ProfileRef{Name: "tier-small", Version: "1.0.0"},
	}
	if _, err := reg.Create(context.Background(), registry.NewRecord(spec, time.Now())); err != nil {
		t.Fatalf("seeding %s: %v", id, err)
	}
	if err := reg.RecordOIDCIssuer(context.Background(), id, issuer); err != nil {
		t.Fatalf("recording issuer for %s: %v", id, err)
	}
}

func validToken(t *testing.T, key *rsa.PrivateKey, issuer, subject, audience string) string {
	t.Helper()
	return signedToken(t, key, jwt.RegisteredClaims{
		Issuer: issuer, Subject: subject,
		Audience: jwt.ClaimStrings{audience}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})
}

func TestHandleStatus_Accepted(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	reg := registry.NewMemory()
	seedClusterWithIssuer(t, reg, "team-a", testIssuer)

	h := NewHandler(reg, NewVerifier(fakeKeyResolver{key: &key.PublicKey}))
	token := validToken(t, key, testIssuer, ExpectedSubject, ExpectedAudience)

	code, resp := h.HandleStatus(context.Background(), "team-a", token, []byte(`{"syncedApps":5}`))
	if code != 202 {
		t.Fatalf("code = %d, want 202; resp = %+v", code, resp)
	}
	if !resp.Accepted {
		t.Errorf("resp = %+v, want Accepted", resp)
	}

	stored, err := reg.Get(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.LastReportedAt.IsZero() {
		t.Error("expected LastReportedAt to have been recorded")
	}
}

func TestHandleStatus_MissingToken(t *testing.T) {
	reg := registry.NewMemory()
	seedClusterWithIssuer(t, reg, "team-a", testIssuer)
	h := NewHandler(reg, NewVerifier(fakeKeyResolver{}))

	code, resp := h.HandleStatus(context.Background(), "team-a", "", nil)
	if code != 401 {
		t.Errorf("code = %d, want 401", code)
	}
	if resp.Error != "missing_token" {
		t.Errorf("resp.Error = %q", resp.Error)
	}
}

func TestHandleStatus_UnknownCluster(t *testing.T) {
	reg := registry.NewMemory()
	h := NewHandler(reg, NewVerifier(fakeKeyResolver{}))

	code, resp := h.HandleStatus(context.Background(), "does-not-exist", "some-token", nil)
	if code != 404 {
		t.Errorf("code = %d, want 404", code)
	}
	if resp.Error != "unknown_cluster" {
		t.Errorf("resp.Error = %q", resp.Error)
	}
}

// The signature this test's token carries verifies fine — against the wrong
// cluster. This is the exact spoofing scenario the anti-replay design exists
// to stop, exercised through the full handler rather than only Verify.
func TestHandleStatus_ReplayedTokenFromAnotherClusterRejected(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	reg := registry.NewMemory()
	seedClusterWithIssuer(t, reg, "team-a", "https://issuer-for-team-a.example.com")
	seedClusterWithIssuer(t, reg, "team-b", "https://issuer-for-team-b.example.com")

	h := NewHandler(reg, NewVerifier(fakeKeyResolver{key: &key.PublicKey}))
	// A genuine token for team-a...
	tokenForA := validToken(t, key, "https://issuer-for-team-a.example.com", ExpectedSubject, ExpectedAudience)

	// ...replayed against team-b's endpoint.
	code, resp := h.HandleStatus(context.Background(), "team-b", tokenForA, nil)
	if code != 403 {
		t.Fatalf("code = %d, want 403; resp = %+v", code, resp)
	}
	if resp.Error != "invalid_token" {
		t.Errorf("resp.Error = %q, want invalid_token", resp.Error)
	}

	stored, err := reg.Get(context.Background(), "team-b")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !stored.LastReportedAt.IsZero() {
		t.Error("team-b's LastReportedAt was updated by a replayed token")
	}
}

func TestHandleStatus_WrongSubjectRejected(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	reg := registry.NewMemory()
	seedClusterWithIssuer(t, reg, "team-a", testIssuer)

	h := NewHandler(reg, NewVerifier(fakeKeyResolver{key: &key.PublicKey}))
	// A valid, correctly-issued token — but for some other in-cluster workload.
	token := validToken(t, key, testIssuer, "system:serviceaccount:default:some-other-pod", ExpectedAudience)

	code, resp := h.HandleStatus(context.Background(), "team-a", token, nil)
	if code != 403 {
		t.Fatalf("code = %d, want 403", code)
	}
	if resp.Error != "wrong_subject" {
		t.Errorf("resp.Error = %q, want wrong_subject", resp.Error)
	}
}

func TestHandleStatus_WrongAudienceRejected(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	reg := registry.NewMemory()
	seedClusterWithIssuer(t, reg, "team-a", testIssuer)

	h := NewHandler(reg, NewVerifier(fakeKeyResolver{key: &key.PublicKey}))
	token := validToken(t, key, testIssuer, ExpectedSubject, "some-other-api")

	code, resp := h.HandleStatus(context.Background(), "team-a", token, nil)
	if code != 403 {
		t.Fatalf("code = %d, want 403", code)
	}
	if resp.Error != "wrong_audience" {
		t.Errorf("resp.Error = %q, want wrong_audience", resp.Error)
	}
}

func TestHandleStatus_InvalidBodyRejected(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	reg := registry.NewMemory()
	seedClusterWithIssuer(t, reg, "team-a", testIssuer)

	h := NewHandler(reg, NewVerifier(fakeKeyResolver{key: &key.PublicKey}))
	token := validToken(t, key, testIssuer, ExpectedSubject, ExpectedAudience)

	code, resp := h.HandleStatus(context.Background(), "team-a", token, []byte(`not json`))
	if code != 400 {
		t.Fatalf("code = %d, want 400", code)
	}
	if resp.Error != "invalid_body" {
		t.Errorf("resp.Error = %q, want invalid_body", resp.Error)
	}
}
