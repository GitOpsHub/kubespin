package fleet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
	"github.com/GitOpsHub/kubespin/internal/registry"
	"github.com/GitOpsHub/kubespin/internal/repo"
)

func timeNow() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func minimalProfile() core.Profile {
	return core.Profile{
		Name:   "small",
		Addons: []core.AddonRef{{Name: "x", Chart: "x", Repository: "https://x", Version: "1.0.0", Namespace: "x"}},
	}
}

func TestAudit_RunsAcrossEveryCluster(t *testing.T) {
	reg := registry.NewMemory()
	repoProv := repo.NewMemory()

	specs := []core.ClusterSpec{auditTestSpec("team-a"), auditTestSpec("team-b")}
	for _, spec := range specs {
		if _, err := reg.Create(context.Background(), registry.NewRecord(spec, timeNow())); err != nil {
			t.Fatalf("seeding %s: %v", spec.ID, err)
		}
		if err := repoProv.Create(context.Background(), spec); err != nil {
			t.Fatalf("creating repo for %s: %v", spec.ID, err)
		}
		checkout, err := repoProv.Clone(context.Background(), spec)
		if err != nil {
			t.Fatalf("Clone: %v", err)
		}
		clusterYAML, _, err := repo.Render(spec, minimalProfile())
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if _, err := repoProv.Push(context.Background(), checkout,
			map[string][]byte{repo.ClusterFile: clusterYAML}, "seed"); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}

	factory := func(context.Context, core.Provider, string) (provisioner.ClusterProvisioner, error) {
		return fakeCluster{state: provisioner.ClusterState{
			Status: provisioner.StatusActive, Access: core.AccessPrivate,
			NodePools: specs[0].NodePools,
		}}, nil
	}

	results, err := Audit(context.Background(), reg, registry.Filter{}, factory, repoProv, 4)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want 2", results)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("cluster %s: %v", r.ClusterID, r.Err)
		}
	}
}

func TestAudit_PersistsFindingsToTheRegistry(t *testing.T) {
	reg := registry.NewMemory()
	repoProv := repo.NewMemory()

	drifted := auditTestSpec("team-drifted")
	if _, err := reg.Create(context.Background(), registry.NewRecord(drifted, timeNow())); err != nil {
		t.Fatalf("seeding registry: %v", err)
	}
	if err := repoProv.Create(context.Background(), drifted); err != nil {
		t.Fatalf("creating repo: %v", err)
	}
	checkout, err := repoProv.Clone(context.Background(), drifted)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	clusterYAML, _, err := repo.Render(drifted, minimalProfile())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, err := repoProv.Push(context.Background(), checkout,
		map[string][]byte{repo.ClusterFile: clusterYAML}, "seed"); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// Live access is public while desired is private, so AuditOne reports
	// exactly one finding.
	factory := func(context.Context, core.Provider, string) (provisioner.ClusterProvisioner, error) {
		return fakeCluster{state: provisioner.ClusterState{
			Status: provisioner.StatusActive, Access: core.AccessPublic,
			NodePools: drifted.NodePools,
		}}, nil
	}

	at := timeNow().Add(time.Hour)
	results, err := Audit(context.Background(), reg, registry.Filter{}, factory, repoProv, 4, WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results = %+v", results)
	}
	if len(results[0].Findings) != 1 {
		t.Fatalf("Findings = %+v, want 1", results[0].Findings)
	}

	stored, err := reg.Get(context.Background(), drifted.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(stored.Findings) != 1 {
		t.Fatalf("registry Findings = %v, want 1 entry", stored.Findings)
	}
	if !stored.FindingsAt.Equal(at) {
		t.Errorf("registry FindingsAt = %v, want %v", stored.FindingsAt, at)
	}
}

func TestAudit_OneClusterFailingDoesNotAbortTheRest(t *testing.T) {
	reg := registry.NewMemory()
	repoProv := repo.NewMemory()

	good := auditTestSpec("team-good")
	bad := auditTestSpec("team-bad")
	for _, spec := range []core.ClusterSpec{good, bad} {
		if _, err := reg.Create(context.Background(), registry.NewRecord(spec, timeNow())); err != nil {
			t.Fatalf("seeding %s: %v", spec.ID, err)
		}
	}
	// Only good's repo is seeded; bad's Clone will fail.
	if err := repoProv.Create(context.Background(), good); err != nil {
		t.Fatalf("Create: %v", err)
	}
	checkout, err := repoProv.Clone(context.Background(), good)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	clusterYAML, _, err := repo.Render(good, minimalProfile())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, err := repoProv.Push(context.Background(), checkout,
		map[string][]byte{repo.ClusterFile: clusterYAML}, "seed"); err != nil {
		t.Fatalf("Push: %v", err)
	}

	factory := func(context.Context, core.Provider, string) (provisioner.ClusterProvisioner, error) {
		return fakeCluster{state: provisioner.ClusterState{
			Status: provisioner.StatusActive, Access: core.AccessPrivate, NodePools: good.NodePools,
		}}, nil
	}

	results, err := Audit(context.Background(), reg, registry.Filter{}, factory, repoProv, 4)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want 2", results)
	}

	var goodOK, badFailed bool
	for _, r := range results {
		switch r.ClusterID {
		case "team-good":
			goodOK = r.Err == nil
		case "team-bad":
			badFailed = r.Err != nil
		}
	}
	if !goodOK {
		t.Error("team-good should have audited successfully")
	}
	if !badFailed {
		t.Error("team-bad should have failed (its repo was never seeded) without aborting the run")
	}
}

func TestAudit_FactoryErrorIsPerCluster(t *testing.T) {
	reg := registry.NewMemory()
	spec := auditTestSpec("team-a")
	if _, err := reg.Create(context.Background(), registry.NewRecord(spec, timeNow())); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	wantErr := errors.New("no credentials configured")
	factory := func(context.Context, core.Provider, string) (provisioner.ClusterProvisioner, error) {
		return nil, wantErr
	}

	results, err := Audit(context.Background(), reg, registry.Filter{}, factory, repo.NewMemory(), 4)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(results) != 1 || !errors.Is(results[0].Err, wantErr) {
		t.Errorf("results = %+v, want the factory error surfaced", results)
	}
}
