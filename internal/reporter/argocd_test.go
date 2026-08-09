package reporter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func fakeArgoCDServer(t *testing.T, wantAuth string, apps applicationList) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/applications" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if wantAuth != "" && r.Header.Get("Authorization") != "Bearer "+wantAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(apps)
	}))
}

func app(sync, health, revision string) application {
	a := application{}
	a.Status.Sync.Status = sync
	a.Status.Sync.Revision = revision
	a.Status.Health.Status = health
	return a
}

func TestHTTPArgoCDClient_Summarize(t *testing.T) {
	srv := fakeArgoCDServer(t, "argocd-token", applicationList{Items: []application{
		app("Synced", "Healthy", "abc123"),
		app("Synced", "Healthy", "abc123"),
		app("OutOfSync", "Progressing", "abc123"),
		app("Synced", "Degraded", "abc123"),
	}})
	defer srv.Close()

	c := NewHTTPArgoCDClient(srv.Client(), srv.URL, "argocd-token")
	summary, err := c.Summarize(context.Background())
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	if summary.SyncedApps != 3 {
		t.Errorf("SyncedApps = %d, want 3", summary.SyncedApps)
	}
	if summary.HealthyApps != 2 {
		t.Errorf("HealthyApps = %d, want 2", summary.HealthyApps)
	}
	if summary.DegradedApps != 1 {
		t.Errorf("DegradedApps = %d, want 1", summary.DegradedApps)
	}
	if summary.CommitSHA != "abc123" {
		t.Errorf("CommitSHA = %q, want abc123", summary.CommitSHA)
	}
}

func TestHTTPArgoCDClient_Summarize_EmptyFleet(t *testing.T) {
	srv := fakeArgoCDServer(t, "", applicationList{})
	defer srv.Close()

	c := NewHTTPArgoCDClient(srv.Client(), srv.URL, "")
	summary, err := c.Summarize(context.Background())
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if summary.SyncedApps != 0 || summary.HealthyApps != 0 || summary.DegradedApps != 0 {
		t.Errorf("summary = %+v, want all zero", summary)
	}
}

func TestHTTPArgoCDClient_Summarize_WrongToken(t *testing.T) {
	srv := fakeArgoCDServer(t, "expected-token", applicationList{})
	defer srv.Close()

	c := NewHTTPArgoCDClient(srv.Client(), srv.URL, "wrong-token")
	if _, err := c.Summarize(context.Background()); err == nil {
		t.Fatal("expected an error with the wrong Argo CD token")
	}
}
