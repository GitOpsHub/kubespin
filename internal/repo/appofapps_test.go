package repo

import (
	"context"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/core"
)

func TestReconcileAppOfApps_FirstCall_Commits(t *testing.T) {
	f := newFakeGitHub()
	p := NewProvisioner(f.clients())
	spec := testSpec()
	profile := testProfile()

	if err := Seed(context.Background(), p, spec, profile); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	changed, err := ReconcileAppOfApps(context.Background(), p, spec, profile)
	if err != nil {
		t.Fatalf("ReconcileAppOfApps: %v", err)
	}
	if !changed {
		t.Fatal("expected the first app-of-apps reconcile to commit")
	}
}

func TestReconcileAppOfApps_NoChange_MakesNoCommit(t *testing.T) {
	f := newFakeGitHub()
	p := NewProvisioner(f.clients())
	spec := testSpec()
	profile := testProfile()

	if err := Seed(context.Background(), p, spec, profile); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if _, err := ReconcileAppOfApps(context.Background(), p, spec, profile); err != nil {
		t.Fatalf("first ReconcileAppOfApps: %v", err)
	}

	f.calls = nil // only the second ReconcileAppOfApps call is under test

	changed, err := ReconcileAppOfApps(context.Background(), p, spec, profile)
	if err != nil {
		t.Fatalf("second ReconcileAppOfApps: %v", err)
	}
	if changed {
		t.Error("expected no change when the profile is unchanged")
	}
	f.assertNoMutations(t)
}

func TestReconcileAppOfApps_AddonChange_Commits(t *testing.T) {
	f := newFakeGitHub()
	p := NewProvisioner(f.clients())
	spec := testSpec()
	profile := testProfile()

	if err := Seed(context.Background(), p, spec, profile); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if _, err := ReconcileAppOfApps(context.Background(), p, spec, profile); err != nil {
		t.Fatalf("first ReconcileAppOfApps: %v", err)
	}

	updated := profile
	updated.Addons = append([]core.AddonRef{}, profile.Addons...)
	updated.Addons[0].Version = "9.9.9"

	changed, err := ReconcileAppOfApps(context.Background(), p, spec, updated)
	if err != nil {
		t.Fatalf("ReconcileAppOfApps: %v", err)
	}
	if !changed {
		t.Fatal("expected an addon version change to be reported as a change")
	}
}

func TestReconcileAppOfApps_PreservesAddonsHash(t *testing.T) {
	// ReconcileAddons and ReconcileAppOfApps write the same .state.yaml file
	// but must not clobber each other's field: a repo that has an addons.yaml
	// hash recorded must keep it after an app-of-apps-only reconcile, and
	// vice versa.
	f := newFakeGitHub()
	p := NewProvisioner(f.clients())
	spec := testSpec()
	profile := testProfile()

	if err := Seed(context.Background(), p, spec, profile); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if _, err := ReconcileAppOfApps(context.Background(), p, spec, profile); err != nil {
		t.Fatalf("ReconcileAppOfApps: %v", err)
	}

	// addons.yaml should still be a no-op after seeding, proving Seed's
	// AddonsHash survived the app-of-apps commit that followed it.
	f.calls = nil
	changed, err := ReconcileAddons(context.Background(), p, spec, profile)
	if err != nil {
		t.Fatalf("ReconcileAddons: %v", err)
	}
	if changed {
		t.Error("expected ReconcileAddons to still be a no-op after an app-of-apps reconcile")
	}

	// And the reverse: after that no-op ReconcileAddons call, app-of-apps
	// should still recognise nothing changed either.
	changed, err = ReconcileAppOfApps(context.Background(), p, spec, profile)
	if err != nil {
		t.Fatalf("ReconcileAppOfApps: %v", err)
	}
	if changed {
		t.Error("expected ReconcileAppOfApps to still be a no-op after a ReconcileAddons no-op")
	}
}
