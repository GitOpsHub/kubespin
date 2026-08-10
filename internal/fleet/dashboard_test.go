package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/registry"
)

func TestDashboard_ReflectsRegistryState(t *testing.T) {
	reg := registry.NewMemory()
	ctx := context.Background()

	stale := auditTestSpec("team-stale")
	fresh := auditTestSpec("team-fresh")
	for _, spec := range []core.ClusterSpec{stale, fresh} {
		rec := registry.NewRecord(spec, timeNow())
		rec.Phase = core.PhaseReady
		if _, err := reg.Create(ctx, rec); err != nil {
			t.Fatalf("seeding %s: %v", spec.ID, err)
		}
	}

	now := timeNow().Add(time.Hour)
	if err := reg.Touch(ctx, fresh.ID, now.Add(-time.Minute)); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if err := reg.RecordFindings(ctx, fresh.ID, nil, now); err != nil {
		t.Fatalf("RecordFindings: %v", err)
	}
	if err := reg.RecordFindings(ctx, stale.ID, []string{"node pool drifted"}, now); err != nil {
		t.Fatalf("RecordFindings: %v", err)
	}

	rows, err := Dashboard(ctx, reg, registry.Filter{}, DefaultStaleThreshold, now)
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}

	// Sorted by ClusterID: team-fresh before team-stale.
	if rows[0].ClusterID != "team-fresh" || rows[1].ClusterID != "team-stale" {
		t.Fatalf("rows not sorted by ClusterID: %+v", rows)
	}

	freshRow, staleRow := rows[0], rows[1]
	if freshRow.Stale {
		t.Error("team-fresh should not be stale: it reported a minute ago")
	}
	if len(freshRow.Findings) != 0 || freshRow.FindingsAt.IsZero() {
		t.Errorf("team-fresh should show a clean, audited result: %+v", freshRow)
	}

	if !staleRow.Stale {
		t.Error("team-stale should be stale: it never reported")
	}
	if len(staleRow.Findings) != 1 {
		t.Errorf("team-stale should carry its one finding: %+v", staleRow)
	}
}
