package reporter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/ingestion"
)

// TokenSource reads the workload identity token fleet-status-reporter
// signs its push with. The real one reads a projected Kubernetes service
// account token from disk; nothing in this package mints or signs a token
// itself — the cluster's own OIDC issuer already did that when the kubelet
// projected it, which is what makes it verifiable against the issuer M2
// bound this component's identity to.
type TokenSource interface {
	Token() (string, error)
}

// FileTokenSource reads a token from a projected volume file, the standard
// Kubernetes pattern for an audience-scoped service account token
// (serviceAccountToken volume projection).
type FileTokenSource struct {
	Path string
}

// Token implements TokenSource.
func (f FileTokenSource) Token() (string, error) {
	b, err := os.ReadFile(f.Path) //nolint:gosec // operator-configured path, by design
	if err != nil {
		return "", fmt.Errorf("reading token from %s: %w", f.Path, err)
	}
	return string(bytes.TrimSpace(b)), nil
}

// Pusher pushes one cluster's status to the Central Ingestion API.
type Pusher struct {
	client    *http.Client
	endpoint  string
	clusterID core.ClusterID
	tokens    TokenSource
}

// NewPusher builds a Pusher that reports clusterID's status to endpoint
// (the Central Ingestion API's base URL, e.g.
// "https://ingest.kubespin.example.com"), signing each push with a token
// from tokens.
func NewPusher(client *http.Client, endpoint string, clusterID core.ClusterID, tokens TokenSource) *Pusher {
	if client == nil {
		client = http.DefaultClient
	}
	return &Pusher{client: client, endpoint: endpoint, clusterID: clusterID, tokens: tokens}
}

// Push queries argocd for a status summary and pushes it.
//
// It reports whether the ingestion API accepted the push; a rejection (bad
// token, unknown cluster) is not a Go error from this method's perspective —
// it is a normal, expected outcome the caller (the CronJob's exit code)
// decides how to react to, the same way HandleStatus on the receiving end
// returns a status code rather than only an error.
func (p *Pusher) Push(ctx context.Context, argocd ArgoCDClient) (accepted bool, err error) {
	summary, err := argocd.Summarize(ctx)
	if err != nil {
		return false, fmt.Errorf("summarizing Argo CD state: %w", err)
	}

	token, err := p.tokens.Token()
	if err != nil {
		return false, fmt.Errorf("reading workload identity token: %w", err)
	}

	payload, err := json.Marshal(ingestion.StatusPayload{
		SyncedApps: summary.SyncedApps, HealthyApps: summary.HealthyApps,
		DegradedApps: summary.DegradedApps, CommitSHA: summary.CommitSHA,
	})
	if err != nil {
		return false, fmt.Errorf("encoding status payload: %w", err)
	}

	url := fmt.Sprintf("%s/clusters/%s/status", p.endpoint, p.clusterID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return false, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("pushing status for %s: %w", p.clusterID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode >= 200 && resp.StatusCode < 300, nil
}
