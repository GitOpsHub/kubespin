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
}

// Status reads the Fleet Registry and reports every matching cluster's
// phase and staleness. It never connects to a cluster — staleness is judged
// entirely from LastReportedAt, which is populated by pushes the Central
// Ingestion API accepts, not by reaching out here.
func Status(
	ctx context.Context, reg registry.Registry, filter registry.Filter, staleOnly bool, threshold time.Duration, now time.Time,
) ([]ClusterStatus, error) {
	records, err := reg.List(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("listing fleet registry: %w", err)
	}

	out := make([]ClusterStatus, 0, len(records))
	for _, rec := range records {
		stale := rec.Stale(now, threshold)
		if staleOnly && !stale {
			continue
		}
		out = append(out, ClusterStatus{
			ClusterID: rec.ClusterID, Provider: rec.Provider, Phase: rec.Phase,
			Stale: stale, LastReportedAt: rec.LastReportedAt,
		})
	}
	return out, nil
}
