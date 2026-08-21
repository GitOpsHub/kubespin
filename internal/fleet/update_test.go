package fleet

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/catalog"
	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/registry"
	"github.com/GitOpsHub/kubespin/internal/repo"
)

func seedClusterRepo(t *testing.T, rp repo.Provisioner, spec core.ClusterSpec, profile core.Profile) {
	t.Helper()
	if err := repo.Seed(context.Background(), rp, spec, profile); err != nil {
		t.Fatalf("seeding repo for %s: %v", spec.ID, err)
	}
}

func builtinTierSmallSpec(id core.ClusterID) core.ClusterSpec {
	spec := auditTestSpec(id)
	spec.Size = core.SizeSmall
	return spec
}

func TestUpdateOne_PinsComponentVersion(t *testing.T) {
	rp := repo.NewMemory()
	resolver := catalog.NewBuiltinResolver()
	spec := builtinTierSmallSpec("team-a")

	profile, err := resolver.Resolve(context.Background(), spec.Size)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	seedClusterRepo(t, rp, spec, profile)

	committed, err := UpdateOne(context.Background(), resolver, rp, spec, "cert-manager", "1.16.0")
	if err != nil {
		t.Fatalf("UpdateOne: %v", err)
	}
	if !committed {
		t.Fatal("expected a version pin to commit")
	}

	checkout, err := rp.Clone(context.Background(), spec)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	addonsYAML, ok := checkout.File(repo.AddonsFile)
	if !ok {
		t.Fatal("expected addons.yaml to exist")
	}
	if !strings.Contains(string(addonsYAML), "1.16.0") {
		t.Errorf("addons.yaml does not reflect the pinned version: %s", addonsYAML)
	}
}

func TestUpdateOne_Idempotent(t *testing.T) {
	rp := repo.NewMemory()
	resolver := catalog.NewBuiltinResolver()
	spec := builtinTierSmallSpec("team-a")

	profile, err := resolver.Resolve(context.Background(), spec.Size)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	seedClusterRepo(t, rp, spec, profile)

	if _, err := UpdateOne(context.Background(), resolver, rp, spec, "cert-manager", "1.16.0"); err != nil {
		t.Fatalf("first UpdateOne: %v", err)
	}

	committed, err := UpdateOne(context.Background(), resolver, rp, spec, "cert-manager", "1.16.0")
	if err != nil {
		t.Fatalf("second UpdateOne: %v", err)
	}
	if committed {
		t.Error("expected the second identical update to be a no-op")
	}
}

func TestUpdateOne_UnknownComponentErrors(t *testing.T) {
	rp := repo.NewMemory()
	resolver := catalog.NewBuiltinResolver()
	spec := builtinTierSmallSpec("team-a")

	profile, err := resolver.Resolve(context.Background(), spec.Size)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	seedClusterRepo(t, rp, spec, profile)

	_, err = UpdateOne(context.Background(), resolver, rp, spec, "does-not-exist", "1.0.0")
	if !errors.Is(err, catalog.ErrUnknownOverride) {
		t.Errorf("error = %v, want one wrapping ErrUnknownOverride", err)
	}
}

func TestUpdate_RunsAcrossEveryMatchingCluster(t *testing.T) {
	reg := registry.NewMemory()
	rp := repo.NewMemory()
	resolver := catalog.NewBuiltinResolver()

	specs := []core.ClusterSpec{builtinTierSmallSpec("team-a"), builtinTierSmallSpec("team-b")}
	profile, err := resolver.Resolve(context.Background(), specs[0].Size)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, spec := range specs {
		if _, err := reg.Create(context.Background(), registry.NewRecord(spec, timeNow())); err != nil {
			t.Fatalf("seeding registry for %s: %v", spec.ID, err)
		}
		seedClusterRepo(t, rp, spec, profile)
	}

	results, err := Update(context.Background(), reg, registry.Filter{}, resolver, rp, "cert-manager", "1.16.0", 4, 0)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want 2", results)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("cluster %s: %v", r.ClusterID, r.Err)
		}
		if !r.Committed {
			t.Errorf("cluster %s: expected a commit", r.ClusterID)
		}
	}
}

func TestUpdate_CanaryWaveFailureSkipsTheRest(t *testing.T) {
	reg := registry.NewMemory()
	rp := repo.NewMemory()
	resolver := catalog.NewBuiltinResolver()

	// team-a sorts first (canary), but its repository is never seeded, so its
	// update fails. team-b is fully seeded and would otherwise succeed.
	specs := []core.ClusterSpec{builtinTierSmallSpec("team-a"), builtinTierSmallSpec("team-b")}
	profile, err := resolver.Resolve(context.Background(), specs[0].Size)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, spec := range specs {
		if _, err := reg.Create(context.Background(), registry.NewRecord(spec, timeNow())); err != nil {
			t.Fatalf("seeding registry for %s: %v", spec.ID, err)
		}
	}
	seedClusterRepo(t, rp, specs[1], profile) // only team-b

	results, err := Update(context.Background(), reg, registry.Filter{}, resolver, rp, "cert-manager", "1.16.0", 4, 1)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want 2", results)
	}

	byID := make(map[core.ClusterID]UpdateResult, len(results))
	for _, r := range results {
		byID[r.ClusterID] = r
	}

	if byID["team-a"].Err == nil {
		t.Error("expected the canary cluster's update to fail")
	}
	if !byID["team-b"].Skipped {
		t.Error("expected the rest of the fleet to be skipped after a canary failure")
	}
	if byID["team-b"].Committed {
		t.Error("a skipped cluster must not have been committed")
	}
}

func TestUpdate_CleanCanaryWaveRollsToTheRest(t *testing.T) {
	reg := registry.NewMemory()
	rp := repo.NewMemory()
	resolver := catalog.NewBuiltinResolver()

	specs := []core.ClusterSpec{builtinTierSmallSpec("team-a"), builtinTierSmallSpec("team-b")}
	profile, err := resolver.Resolve(context.Background(), specs[0].Size)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, spec := range specs {
		if _, err := reg.Create(context.Background(), registry.NewRecord(spec, timeNow())); err != nil {
			t.Fatalf("seeding registry for %s: %v", spec.ID, err)
		}
		seedClusterRepo(t, rp, spec, profile)
	}

	results, err := Update(context.Background(), reg, registry.Filter{}, resolver, rp, "cert-manager", "1.16.0", 4, 1)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("cluster %s: %v", r.ClusterID, r.Err)
		}
		if r.Skipped {
			t.Errorf("cluster %s: should not have been skipped after a clean canary wave", r.ClusterID)
		}
		if !r.Committed {
			t.Errorf("cluster %s: expected a commit", r.ClusterID)
		}
	}
}

func TestUpdate_PreservesExistingOverrides(t *testing.T) {
	rp := repo.NewMemory()
	resolver := catalog.NewBuiltinResolver()
	reg := registry.NewMemory()

	spec := builtinTierSmallSpec("team-a")
	spec.Overrides = []core.AddonOverride{{Name: "fleet-status-reporter", Version: "0.2.0"}}
	profile, err := resolver.Resolve(context.Background(), spec.Size)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	seedClusterRepo(t, rp, spec, profile)
	if _, err := reg.Create(context.Background(), registry.NewRecord(spec, timeNow())); err != nil {
		t.Fatalf("seeding registry: %v", err)
	}

	if _, err := Update(context.Background(), reg, registry.Filter{}, resolver, rp, "cert-manager", "1.16.0", 4, 0); err != nil {
		t.Fatalf("Update: %v", err)
	}

	checkout, err := rp.Clone(context.Background(), spec)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	addonsYAML, _ := checkout.File(repo.AddonsFile)
	if !strings.Contains(string(addonsYAML), "0.2.0") {
		t.Errorf("existing override was lost: %s", addonsYAML)
	}
	if !strings.Contains(string(addonsYAML), "1.16.0") {
		t.Errorf("new update was not applied: %s", addonsYAML)
	}
}
