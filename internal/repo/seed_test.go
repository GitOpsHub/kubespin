package repo

import (
	"context"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/core"
)

func TestSeed_CreatesAndSeedsRepo(t *testing.T) {
	f := newFakeGitHub()
	p := NewProvisioner(f.clients())
	spec := testSpec()
	profile := testProfile()

	if err := Seed(context.Background(), p, spec, profile); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	checkout, err := p.Clone(context.Background(), spec)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}

	clusterYAML, ok := checkout.File(ClusterFile)
	if !ok || len(clusterYAML) == 0 {
		t.Fatal("expected cluster.yaml to be seeded")
	}
	addonsYAML, ok := checkout.File(AddonsFile)
	if !ok || len(addonsYAML) == 0 {
		t.Fatal("expected addons.yaml to be seeded")
	}
	stateYAML, ok := checkout.File(StateFile)
	if !ok || len(stateYAML) == 0 {
		t.Fatal("expected .state.yaml to be seeded")
	}
}

func TestSeed_Idempotent_SecondCallCommitsNothing(t *testing.T) {
	f := newFakeGitHub()
	p := NewProvisioner(f.clients())
	spec := testSpec()
	profile := testProfile()

	if err := Seed(context.Background(), p, spec, profile); err != nil {
		t.Fatalf("first Seed: %v", err)
	}
	commitsAfterFirst := countCalls(f, "CreateCommit")

	if err := Seed(context.Background(), p, spec, profile); err != nil {
		t.Fatalf("second Seed: %v", err)
	}

	if got := countCalls(f, "CreateCommit"); got != commitsAfterFirst {
		t.Errorf("second Seed made %d more commits, want 0", got-commitsAfterFirst)
	}
}

func TestReconcileAddons_NoChange_MakesNoCommit(t *testing.T) {
	f := newFakeGitHub()
	p := NewProvisioner(f.clients())
	spec := testSpec()
	profile := testProfile()

	if err := Seed(context.Background(), p, spec, profile); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	f.calls = nil // only the ReconcileAddons call below is under test

	changed, err := ReconcileAddons(context.Background(), p, spec, profile)
	if err != nil {
		t.Fatalf("ReconcileAddons: %v", err)
	}
	if changed {
		t.Error("expected no change when the profile is unchanged")
	}
	f.assertNoMutations(t)
}

func TestReconcileAddons_AddonValueChange_Commits(t *testing.T) {
	f := newFakeGitHub()
	p := NewProvisioner(f.clients())
	spec := testSpec()
	profile := testProfile()

	if err := Seed(context.Background(), p, spec, profile); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	updated := profile
	updated.Addons = append([]core.AddonRef{}, profile.Addons...)
	updated.Addons[0].Version = "1.16.0" // changed value

	changed, err := ReconcileAddons(context.Background(), p, spec, updated)
	if err != nil {
		t.Fatalf("ReconcileAddons: %v", err)
	}
	if !changed {
		t.Fatal("expected an addon version change to be reported as a change")
	}

	checkout, err := p.Clone(context.Background(), spec)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	addonsYAML, _ := checkout.File(AddonsFile)
	if !contains(string(addonsYAML), "1.16.0") {
		t.Errorf("addons.yaml does not reflect the updated version: %s", addonsYAML)
	}
}

func TestReconcileAddons_InfraOnlyChange_NoCommit(t *testing.T) {
	f := newFakeGitHub()
	p := NewProvisioner(f.clients())
	spec := testSpec()
	profile := testProfile()

	if err := Seed(context.Background(), p, spec, profile); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	// A node pool resize changes the spec but not the resolved profile — the
	// scenario the M3 acceptance criteria calls out explicitly: this must
	// reconcile via the cloud SDK (exercised in internal/provisioner tests),
	// never via a git commit.
	resized := spec
	resized.NodePools = append([]core.NodePool{}, spec.NodePools...)
	resized.NodePools[0].DesiredSize = 4

	changed, err := ReconcileAddons(context.Background(), p, resized, profile)
	if err != nil {
		t.Fatalf("ReconcileAddons: %v", err)
	}
	if changed {
		t.Error("expected an infra-only change to produce no git commit")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
