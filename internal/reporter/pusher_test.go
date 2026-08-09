package reporter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/ingestion"
)

// fakeArgoCD is a fixed ArgoCDClient, for Pusher tests that don't need to
// exercise the real HTTP client (that's HTTPArgoCDClient's own tests).
type fakeArgoCD struct {
	summary Summary
	err     error
}

func (f fakeArgoCD) Summarize(context.Context) (Summary, error) { return f.summary, f.err }

type fixedToken struct {
	token string
	err   error
}

func (f fixedToken) Token() (string, error) { return f.token, f.err }

func fakeIngestionServer(t *testing.T, wantClusterID, wantToken string, status int) (*httptest.Server, *ingestion.StatusPayload) {
	t.Helper()
	var received ingestion.StatusPayload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/clusters/" + wantClusterID + "/status"
		if r.URL.Path != wantPath {
			t.Errorf("path = %s, want %s", r.URL.Path, wantPath)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantToken {
			t.Errorf("Authorization = %q, want Bearer %s", got, wantToken)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decoding pushed payload: %v", err)
		}
		w.WriteHeader(status)
	}))
	return srv, &received
}

func TestPusher_Push_Accepted(t *testing.T) {
	srv, received := fakeIngestionServer(t, "team-a", "sa-token", http.StatusAccepted)
	defer srv.Close()

	p := NewPusher(srv.Client(), srv.URL, core.ClusterID("team-a"), fixedToken{token: "sa-token"})
	argocd := fakeArgoCD{summary: Summary{SyncedApps: 5, HealthyApps: 5, CommitSHA: "abc123"}}

	accepted, err := p.Push(context.Background(), argocd)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !accepted {
		t.Error("expected the push to be accepted")
	}
	if received.SyncedApps != 5 || received.CommitSHA != "abc123" {
		t.Errorf("received = %+v, want it to carry the summary", received)
	}
}

func TestPusher_Push_Rejected(t *testing.T) {
	srv, _ := fakeIngestionServer(t, "team-a", "sa-token", http.StatusForbidden)
	defer srv.Close()

	p := NewPusher(srv.Client(), srv.URL, core.ClusterID("team-a"), fixedToken{token: "sa-token"})
	accepted, err := p.Push(context.Background(), fakeArgoCD{})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if accepted {
		t.Error("expected the push to be reported as not accepted")
	}
}

func TestPusher_Push_ArgoCDErrorSurfaces(t *testing.T) {
	p := NewPusher(nil, "https://ingest.example.com", core.ClusterID("team-a"), fixedToken{token: "x"})
	argocd := fakeArgoCD{err: context.DeadlineExceeded}

	if _, err := p.Push(context.Background(), argocd); err == nil {
		t.Fatal("expected an Argo CD error to surface")
	}
}

func TestPusher_Push_MissingTokenSurfaces(t *testing.T) {
	p := NewPusher(nil, "https://ingest.example.com", core.ClusterID("team-a"), fixedToken{err: context.DeadlineExceeded})

	if _, err := p.Push(context.Background(), fakeArgoCD{}); err == nil {
		t.Fatal("expected a token read error to surface")
	}
}
