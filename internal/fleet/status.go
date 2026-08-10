// Package fleet implements the fleet-wide operations: status, audit, and
// update. Unlike internal/orchestrator, which drives one cluster at a time,
// everything here starts from a Fleet Registry listing and fans out.
package fleet

import (
	"context"
	"fmt"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/registry"
)

// DefaultStaleThreshold is how long a ready cluster can go without reporting
// before `fleet status` flags it stale. fleet-status-reporter pushes every
// 2-3 minutes (M6), so this tolerates a handful of missed pushes before
// treating a gap as a real problem rather than one delayed heartbeat.
const DefaultStaleThreshold = 10 * time.Minute

// ClusterStatus is one cluster's row in a `fleet status` report.
type ClusterStatus struct {
	ClusterID      core.ClusterID
	Provider       core.Provider
	Phase          core.Phase
	Stale          bool
	LastReportedAt time.Time

	// FindingsCount and FindingsAt reflect the cluster's most recent `fleet
	// audit` run, read straight off the Fleet Registry record. FindingsAt
	// zero means the cluster has never been audited; that is distinct from
	// FindingsCount 0, which means the last audit found no drift.
	FindingsCount int
	FindingsAt    time.Time
}

// Status reads the Fleet Registry and reports every matching cluster's
// phase and staleness. It never connects to a cluster — staleness is judged
// entirely from LastReportedAt, which is populated by pushes the Central
// Ingestion API accepts, not by reaching out here.
func Status(
	ctx context.Context, reg registry.Registry, filter registry.Filter, staleOnly bool, threshold time.Duration, now time.Time,
	opts ...Option,
) ([]ClusterStatus, error) {
	logger := resolveOptions(opts).logger

	records, err := reg.List(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("listing fleet registry: %w", err)
	}
	logger.Debug("reporting fleet status",
		"clusters", len(records), "provider_filter", filter.Provider, "phase_filter", filter.Phase,
		"stale_only", staleOnly, "stale_threshold", threshold)

	out := make([]ClusterStatus, 0, len(records))
	for _, rec := range records {
		stale := rec.Stale(now, threshold)
		if stale {
			logger.Debug("cluster is stale",
				"cluster", rec.ClusterID, "phase", rec.Phase, "last_reported_at", rec.LastReportedAt)
		}
		if staleOnly && !stale {
			logger.Debug("cluster skipped: not stale", "cluster", rec.ClusterID)
			continue
		}
		out = append(out, ClusterStatus{
			ClusterID: rec.ClusterID, Provider: rec.Provider, Phase: rec.Phase,
			Stale: stale, LastReportedAt: rec.LastReportedAt,
			FindingsCount: len(rec.Findings), FindingsAt: rec.FindingsAt,
		})
	}
	return out, nil
}
