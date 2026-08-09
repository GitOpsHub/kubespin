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
	"log"
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
	if err := run(); err != nil {
		log.Fatalf("fleet-status-reporter: %v", err)
	}
}

func run() error {
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

	log.Printf("fleet-status-reporter: pushed status for %s", clusterID)
	return nil
}
