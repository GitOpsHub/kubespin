// Package reporter implements fleet-status-reporter: the in-cluster CronJob
// that queries the cluster's local Argo CD instance and pushes a compact,
// signed status summary to the Central Ingestion API.
//
// It is the outbound half of this project's outbound-only architecture:
// nothing ever reaches into a cluster, so this is the only way a cluster's
// state escapes it.
package reporter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

// options carries settings shared by this package's constructors.
type options struct {
	logger *slog.Logger
}

// Option configures a reporter component.
type Option func(*options)

// WithLogger sets the logger. Without it, a component logs to slog.Default().
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) {
		if logger != nil {
			o.logger = logger
		}
	}
}

// resolve applies opts over the defaults.
func resolve(opts []Option) options {
	o := options{logger: slog.Default()}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// loggerOr keeps a component built as a bare struct literal from panicking on
// a nil logger.
func loggerOr(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.Default()
	}
	return logger
}

// Summary is the compact status this package extracts from Argo CD: counts,
// not the full application list. The Central Ingestion API and Fleet
// Registry only ever need to know "is this cluster healthy", not every
// resource of every Application.
type Summary struct {
	SyncedApps   int
	HealthyApps  int
	DegradedApps int
	CommitSHA    string
}

// ArgoCDClient summarizes the local Argo CD instance's application state.
type ArgoCDClient interface {
	Summarize(ctx context.Context) (Summary, error)
}

// applicationList mirrors the subset of Argo CD's
// GET /api/v1/applications response this package reads.
type applicationList struct {
	Items []application `json:"items"`
}

type application struct {
	Status struct {
		Sync struct {
			Status   string `json:"status"`
			Revision string `json:"revision"`
		} `json:"sync"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
	} `json:"status"`
}

// HTTPArgoCDClient calls the local Argo CD server's REST API over the
// in-cluster network — the one connection this whole architecture allows
// inbound-to-the-namespace but never inbound-to-the-cluster-from-outside,
// since fleet-status-reporter and Argo CD are both inside it.
type HTTPArgoCDClient struct {
	client  *http.Client
	baseURL string
	token   string
	logger  *slog.Logger
}

// NewHTTPArgoCDClient builds a client against baseURL (typically
// "https://argocd-server.argocd.svc:443"), authenticating with token — Argo
// CD's own API token, not the workload identity token this package later
// signs its push to the ingestion API with; the two prove different things
// to different services.
func NewHTTPArgoCDClient(client *http.Client, baseURL, token string, opts ...Option) *HTTPArgoCDClient {
	if client == nil {
		client = http.DefaultClient
	}
	o := resolve(opts)
	return &HTTPArgoCDClient{client: client, baseURL: baseURL, token: token, logger: o.logger}
}

// Summarize implements ArgoCDClient.
func (c *HTTPArgoCDClient) Summarize(ctx context.Context) (Summary, error) {
	logger := loggerOr(c.logger)
	logger.Info("querying local Argo CD", "url", c.baseURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/applications", nil)
	if err != nil {
		return Summary{}, fmt.Errorf("building request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return Summary{}, fmt.Errorf("requesting applications: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Summary{}, fmt.Errorf("requesting applications: unexpected status %d", resp.StatusCode)
	}

	var list applicationList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return Summary{}, fmt.Errorf("decoding applications: %w", err)
	}

	summary := summarize(list)
	logger.Info("summarized Argo CD applications",
		"applications", len(list.Items), "synced", summary.SyncedApps,
		"healthy", summary.HealthyApps, "degraded", summary.DegradedApps,
		"commit", summary.CommitSHA)
	if summary.DegradedApps > 0 {
		logger.Warn("Argo CD reports degraded applications", "degraded", summary.DegradedApps)
	}
	return summary, nil
}

// summarize counts application by sync/health status. Degraded is counted
// deliberately narrowly (Argo CD also reports Progressing, Missing,
// Unknown): those are transient or informational, and folding them into
// "degraded" would make fleet status noisy on every routine rollout.
func summarize(list applicationList) Summary {
	var s Summary
	for _, app := range list.Items {
		if app.Status.Sync.Status == "Synced" {
			s.SyncedApps++
			if s.CommitSHA == "" {
				s.CommitSHA = app.Status.Sync.Revision
			}
		}
		if app.Status.Health.Status == "Healthy" {
			s.HealthyApps++
		}
		if app.Status.Health.Status == "Degraded" {
			s.DegradedApps++
		}
	}
	return s
}
