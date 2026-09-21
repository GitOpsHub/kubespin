package aws

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

// captureTransport records the last request it saw and returns a canned
// response, standing in for the real network round trip in these tests.
type captureTransport struct {
	got *http.Request
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.got = req
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func TestRESTConfig(t *testing.T) {
	t.Run("active cluster yields a usable config", func(t *testing.T) {
		f := newFakeAWS()
		spec := testSpec()
		f.activeCluster(spec)

		cfg, err := NewClusterProvisioner(f.clients()).RESTConfig(t.Context(), spec)
		if err != nil {
			t.Fatalf("RESTConfig: %v", err)
		}
		if cfg.Host != "https://example.eks.amazonaws.com" {
			t.Errorf("Host = %q, want the cluster's endpoint", cfg.Host)
		}
		if string(cfg.CAData) != "fake-ca-cert" {
			t.Errorf("CAData = %q, want the decoded cluster CA", cfg.CAData)
		}
		if cfg.WrapTransport == nil {
			t.Fatal("expected WrapTransport to mint the bearer token lazily, per request")
		}

		// No token is minted merely by building the config...
		if f.called("PresignGetCallerIdentity") {
			t.Error("expected no bearer token minted before the first request")
		}

		// ...only once something actually makes a request through it.
		capture := &captureTransport{}
		rt := cfg.WrapTransport(capture)
		req, err := http.NewRequest(http.MethodGet, cfg.Host+"/version", nil)
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		if _, err := rt.RoundTrip(req); err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		if !f.called("PresignGetCallerIdentity") {
			t.Error("expected a bearer token to be minted via STS on first request")
		}
		auth := capture.got.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer "+eksTokenPrefix) {
			t.Errorf("Authorization = %q, want it to carry a token prefixed with %q", auth, eksTokenPrefix)
		}
	})

	t.Run("absent cluster is an error", func(t *testing.T) {
		f := newFakeAWS()

		_, err := NewClusterProvisioner(f.clients()).RESTConfig(t.Context(), testSpec())
		if err == nil {
			t.Fatal("expected an error for a cluster that does not exist yet")
		}
	})

	t.Run("not-yet-active cluster is an error", func(t *testing.T) {
		f := newFakeAWS()
		spec := testSpec()
		f.activeCluster(spec)
		f.cluster.Status = ekstypes.ClusterStatusCreating

		_, err := NewClusterProvisioner(f.clients()).RESTConfig(t.Context(), spec)
		if err == nil {
			t.Fatal("expected an error for a cluster that is not active yet")
		}
	})

	_ = provisioner.RESTConfigProvisioner(&ClusterProvisioner{})
}

func TestEKSRefreshingTransport_ReusesTokenWithinInterval(t *testing.T) {
	f := newFakeAWS()
	now := time.Now()
	rt := &eksRefreshingTransport{
		base:    &captureTransport{},
		sts:     f,
		cluster: "team-payments-prod",
		now:     func() time.Time { return now },
	}

	req, _ := http.NewRequest(http.MethodGet, "https://example.eks.amazonaws.com/version", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("first RoundTrip: %v", err)
	}
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("second RoundTrip: %v", err)
	}

	calls := 0
	for _, c := range f.calls {
		if c == "PresignGetCallerIdentity" {
			calls++
		}
	}
	if calls != 1 {
		t.Errorf("STS calls = %d, want exactly 1 (second request should reuse the cached token)", calls)
	}
}

func TestEKSRefreshingTransport_RefreshesAfterInterval(t *testing.T) {
	f := newFakeAWS()
	now := time.Now()
	rt := &eksRefreshingTransport{
		base:    &captureTransport{},
		sts:     f,
		cluster: "team-payments-prod",
		now:     func() time.Time { return now },
	}

	req, _ := http.NewRequest(http.MethodGet, "https://example.eks.amazonaws.com/version", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("first RoundTrip: %v", err)
	}

	now = now.Add(eksTokenRefreshInterval + time.Second)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("second RoundTrip: %v", err)
	}

	calls := 0
	for _, c := range f.calls {
		if c == "PresignGetCallerIdentity" {
			calls++
		}
	}
	if calls != 2 {
		t.Errorf("STS calls = %d, want exactly 2 (token should be re-minted once the interval elapses)", calls)
	}
}
