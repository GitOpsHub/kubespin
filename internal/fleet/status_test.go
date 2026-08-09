package fleet

import (
	"testing"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/registry"
)

func seedRecord(t *testing.T, reg registry.Registry, id core.ClusterID, provider core.Provider, now time.Time) registry.Record {
	t.Helper()

	spec := core.ClusterSpec{
		ID: id, Provider: provider, Region: "us-east-1", Access: core.AccessPrivate,
		Profile: core.ProfileRef{Name: "tier-small", Version: "1.0.0"},
	}
	rec, err := reg.Create(t.Context(), registry.NewRecord(spec, now))
	if err != nil {
		t.Fatalf("seeding %s: %v", id, err)
	}
	return rec
}

func advanceToReady(t *testing.T, reg registry.Registry, rec registry.Record) registry.Record {
	t.Helper()
	for _, phase := range []core.Phase{
		core.PhaseClusterCreated, core.PhaseIdentityBound, core.PhaseRepoPushed,
		core.PhaseArgoCDInstalled, core.PhaseReady,
	} {
		var err error
		rec, err = reg.UpdatePhase(t.Context(), rec, phase)
		if err != nil {
			t.Fatalf("advancing to %s: %v", phase, err)
		}
	}
	return rec
}

func TestStatus_ReportsEveryCluster(t *testing.T) {
	reg := registry.NewMemory()
	now := time.Now()

	seedRecord(t, reg, "team-a", core.ProviderAWS, now)
	seedRecord(t, reg, "team-b", core.ProviderGCP, now)

	statuses, err := Status(t.Context(), reg, registry.Filter{}, false, DefaultStaleThreshold, now)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("statuses = %+v, want 2", statuses)
	}
}

func TestStatus_StaleOnlyFiltersOutFreshClusters(t *testing.T) {
	reg := registry.NewMemory()
	now := time.Now()

	fresh := seedRecord(t, reg, "team-fresh", core.ProviderAWS, now)
	fresh = advanceToReady(t, reg, fresh)
	if err := reg.Touch(t.Context(), fresh.ClusterID, now); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	stale := seedRecord(t, reg, "team-stale", core.ProviderAWS, now.Add(-time.Hour))
	advanceToReady(t, reg, stale)
	// Never touched — stale from CreatedAt.

	statuses, err := Status(t.Context(), reg, registry.Filter{}, true, DefaultStaleThreshold, now)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(statuses) != 1 || statuses[0].ClusterID != "team-stale" {
		t.Errorf("statuses = %+v, want only team-stale", statuses)
	}
}

func TestStatus_FiltersByProvider(t *testing.T) {
	reg := registry.NewMemory()
	now := time.Now()

	seedRecord(t, reg, "team-aws", core.ProviderAWS, now)
	seedRecord(t, reg, "team-gcp", core.ProviderGCP, now)

	statuses, err := Status(t.Context(), reg, registry.Filter{Provider: core.ProviderGCP}, false, DefaultStaleThreshold, now)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(statuses) != 1 || statuses[0].ClusterID != "team-gcp" {
		t.Errorf("statuses = %+v, want only team-gcp", statuses)
	}
}
