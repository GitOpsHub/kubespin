package fleet

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/registry"
)

// DashboardRow is one cluster's row in `fleet dashboard`: sync/phase status,
// staleness, and the full drift findings from its most recent `fleet audit`
// run, all read straight off its Fleet Registry record and correlated by
// ClusterID.
//
// A commit SHA is deliberately not part of this: the Fleet Registry does not
// track one (a cluster's resolved addons and .state.yaml live in its own
// repository, not centrally — see internal/registry.Record's doc comment),
// so correlating by SHA would mean a repository read per cluster that
// nothing else this command does requires. ClusterID is the correlation key
// the registry and every cluster's own repository already share.
type DashboardRow struct {
	ClusterID      core.ClusterID
	Provider       core.Provider
	Phase          core.Phase
	Stale          bool
	LastReportedAt time.Time
	Findings       []string
	FindingsAt     time.Time
}

// Dashboard reads the Fleet Registry and returns one row per matching
// cluster, ordered by ClusterID so a rendered report is stable run to run.
// Like Status, it never connects to a cluster: every field comes from the
// registry record fleet-status-reporter and fleet audit already populated.
func Dashboard(
	ctx context.Context, reg registry.Registry, filter registry.Filter, threshold time.Duration, now time.Time,
	opts ...Option,
) ([]DashboardRow, error) {
	logger := resolveOptions(opts).logger

	records, err := reg.List(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("listing fleet registry: %w", err)
	}
	logger.Debug("building fleet dashboard",
		"clusters", len(records), "provider_filter", filter.Provider, "phase_filter", filter.Phase)

	sort.Slice(records, func(i, j int) bool { return records[i].ClusterID < records[j].ClusterID })

	rows := make([]DashboardRow, 0, len(records))
	for _, rec := range records {
		rows = append(rows, DashboardRow{
			ClusterID:      rec.ClusterID,
			Provider:       rec.Provider,
			Phase:          rec.Phase,
			Stale:          rec.Stale(now, threshold),
			LastReportedAt: rec.LastReportedAt,
			Findings:       rec.Findings,
			FindingsAt:     rec.FindingsAt,
		})
	}
	return rows, nil
}
