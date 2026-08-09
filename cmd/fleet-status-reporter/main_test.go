package main

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRun_MissingEnv(t *testing.T) {
	t.Setenv(envClusterID, "")
	t.Setenv(envArgoCDServer, "")
	t.Setenv(envIngestionURL, "")

	if err := run(testLogger()); !errors.Is(err, errRequiredEnv) {
		t.Fatalf("error = %v, want errRequiredEnv", err)
	}
}

func TestRun_PushesStatus(t *testing.T) {
	argoCD := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer argoCD.Close()

	var pushedPath string
	ingestion := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushedPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ingestion.Close()

	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("sa-token\n"), 0o600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}

	t.Setenv(envClusterID, "team-a")
	t.Setenv(envArgoCDServer, argoCD.URL)
	t.Setenv(envIngestionURL, ingestion.URL)
	t.Setenv(envTokenPath, tokenPath)

	if err := run(testLogger()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if pushedPath != "/clusters/team-a/status" {
		t.Errorf("pushed path = %q", pushedPath)
	}
}

func TestRun_RejectedPushSurfaces(t *testing.T) {
	argoCD := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer argoCD.Close()

	ingestion := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ingestion.Close()

	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("sa-token"), 0o600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}

	t.Setenv(envClusterID, "team-a")
	t.Setenv(envArgoCDServer, argoCD.URL)
	t.Setenv(envIngestionURL, ingestion.URL)
	t.Setenv(envTokenPath, tokenPath)

	if err := run(testLogger()); !errors.Is(err, errRejected) {
		t.Fatalf("error = %v, want errRejected", err)
	}
}

// testLogger discards output so test runs stay quiet.
func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
