package ingestion

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// jwksServer is a stand-in for a cluster's cloud-hosted OIDC issuer: it
// serves discovery + JWKS, counts fetches, and can rotate its key.
type jwksServer struct {
	*httptest.Server

	mu      sync.Mutex
	keys    []jwk
	fetches atomic.Int64
}

func newJWKSServer(t *testing.T, kid string) *jwksServer {
	t.Helper()

	s := &jwksServer{}
	s.rotateTo(t, kid)

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(oidcDiscovery{JWKSURI: s.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		s.fetches.Add(1)

		s.mu.Lock()
		defer s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(jwkSet{Keys: s.keys})
	})

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// rotateTo replaces the served key set with a single key named kid, the way
// a cloud provider rotates a cluster's OIDC signing key.
func (s *jwksServer) rotateTo(t *testing.T, kid string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = []jwk{{
		Kid: kid,
		Kty: "RSA",
		Alg: "RS256",
		N:   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}
}

// TestJWKSResolver_CachesAcrossCalls keeps the refetch-on-miss behaviour from
// costing a round trip on every single status push.
func TestJWKSResolver_CachesAcrossCalls(t *testing.T) {
	s := newJWKSServer(t, "key-1")
	r := NewJWKSResolver(s.Client())

	for range 5 {
		if _, err := r.Resolve(context.Background(), s.URL, "key-1"); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}

	if got := s.fetches.Load(); got != 1 {
		t.Errorf("JWKS fetched %d times, want 1 — the cache is not being used", got)
	}
}

// TestJWKSResolver_RefetchesAfterKeyRotation is the regression test for a
// silent per-cluster outage: the cached key set never being revisited meant
// that once the issuer rotated, every push from that cluster was rejected as
// an invalid token until the process restarted.
func TestJWKSResolver_RefetchesAfterKeyRotation(t *testing.T) {
	s := newJWKSServer(t, "key-1")
	r := NewJWKSResolver(s.Client())

	if _, err := r.Resolve(context.Background(), s.URL, "key-1"); err != nil {
		t.Fatalf("Resolve before rotation: %v", err)
	}

	s.rotateTo(t, "key-2")

	// The rate limiter would otherwise suppress the refetch; a real rotation
	// is separated from the previous fetch by far more than this.
	r.now = func() time.Time { return time.Now().Add(2 * jwksMinRefetchInterval) }

	if _, err := r.Resolve(context.Background(), s.URL, "key-2"); err != nil {
		t.Fatalf("Resolve after rotation: %v, want the new key to be fetched", err)
	}
	if got := s.fetches.Load(); got != 2 {
		t.Errorf("JWKS fetched %d times, want 2 — the rotation did not trigger a refetch", got)
	}
}

// TestJWKSResolver_RateLimitsRefetches stops tokens bearing garbage key IDs
// from turning into a stream of outbound requests to the issuer.
func TestJWKSResolver_RateLimitsRefetches(t *testing.T) {
	s := newJWKSServer(t, "key-1")
	r := NewJWKSResolver(s.Client())

	for range 10 {
		_, err := r.Resolve(context.Background(), s.URL, "bogus-kid")
		if err == nil {
			t.Fatal("Resolve succeeded for an unknown kid, want an error")
		}
		if !strings.Contains(err.Error(), "no signing key") {
			t.Fatalf("error = %v, want it to report an unknown signing key", err)
		}
	}

	if got := s.fetches.Load(); got != 1 {
		t.Errorf("JWKS fetched %d times for an unknown kid, want 1", got)
	}
}

// TestJWKSResolver_KeepsCachedKeysWhenTheIssuerIsUnreachable keeps a brief
// network failure from being treated as evidence the keys changed.
func TestJWKSResolver_KeepsCachedKeysWhenTheIssuerIsUnreachable(t *testing.T) {
	s := newJWKSServer(t, "key-1")
	r := NewJWKSResolver(s.Client())

	if _, err := r.Resolve(context.Background(), s.URL, "key-1"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	s.Close() // the issuer goes away

	// Past the cache TTL, so the next Resolve must try to refetch and fail.
	r.now = func() time.Time { return time.Now().Add(2 * jwksCacheTTL) }

	if _, err := r.Resolve(context.Background(), s.URL, "key-1"); err != nil {
		t.Errorf("Resolve: %v, want the cached key to be used while the issuer is unreachable", err)
	}
}

// TestJWKSResolver_ConcurrentResolvesAreSafe guards the cache against the
// fatal "concurrent map writes" panic it would otherwise take the first time
// this resolver is served from anything concurrent.
func TestJWKSResolver_ConcurrentResolvesAreSafe(t *testing.T) {
	s := newJWKSServer(t, "key-1")
	r := NewJWKSResolver(s.Client())

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.Resolve(context.Background(), s.URL, "key-1")
		}()
	}
	wg.Wait()
}
