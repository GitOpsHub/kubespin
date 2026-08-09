// Command fleet-status-reporter runs as an in-cluster CronJob: it queries
// the cluster's local Argo CD instance, builds a compact status summary, and
// pushes it to the Central Ingestion API, signed with the cluster's
// workload identity token.
//
// It is deliberately a single push per invocation rather than a long-running
// loop — the Kubernetes CronJob resource owns the 2-3 minute schedule (see
// the implementation plan's Milestone 6), so this binary's whole job is to
// run once, push once, and report success or failure through its exit code.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/reporter"
)

var (
	errRequiredEnv = errors.New("CLUSTER_ID, ARGOCD_SERVER, and INGESTION_URL must all be set")
	errRejected    = errors.New("the Central Ingestion API rejected the push")
)

// Environment variables this CronJob is configured with. All required:
// there is no sensible default for which cluster or which Argo CD to report
// on, the way there is none for the cloud credentials cluster provisioning
// commands need.
const (
	envClusterID    = "CLUSTER_ID"
	envArgoCDServer = "ARGOCD_SERVER"
	envArgoCDToken  = "ARGOCD_TOKEN" //nolint:gosec // this is an env var name, not a credential
	envIngestionURL = "INGESTION_URL"
	envTokenPath    = "IDENTITY_TOKEN_PATH"
	//nolint:gosec // this is a file path to a projected volume, not a credential
	defaultTokenPath    = "/var/run/secrets/kubespin/token"
	defaultPushDeadline = 30 * time.Second
)

func main() {
	// JSON, because this runs as a CronJob pod whose stderr is scraped into
	// the cluster's log pipeline, where structured fields stay queryable.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(logger); err != nil {
		logger.Error("fleet-status-reporter failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	clusterID := os.Getenv(envClusterID)
	argoCDServer := os.Getenv(envArgoCDServer)
	ingestionURL := os.Getenv(envIngestionURL)
	if clusterID == "" || argoCDServer == "" || ingestionURL == "" {
		return errRequiredEnv
	}

	tokenPath := os.Getenv(envTokenPath)
	if tokenPath == "" {
		tokenPath = defaultTokenPath
	}

	logger.Info("pushing cluster status",
		"cluster", clusterID,
		"argocd_server", argoCDServer,
		"ingestion_url", ingestionURL,
	)

	ctx, cancel := context.WithTimeout(context.Background(), defaultPushDeadline)
	defer cancel()

	argocd := reporter.NewHTTPArgoCDClient(nil, argoCDServer, os.Getenv(envArgoCDToken))
	pusher := reporter.NewPusher(nil, ingestionURL, core.ClusterID(clusterID), reporter.FileTokenSource{Path: tokenPath})

	accepted, err := pusher.Push(ctx, argocd)
	if err != nil {
		return fmt.Errorf("pushing status: %w", err)
	}
	if !accepted {
		return errRejected
	}

	logger.Info("status accepted by the Central Ingestion API", "cluster", clusterID)
	return nil
}
