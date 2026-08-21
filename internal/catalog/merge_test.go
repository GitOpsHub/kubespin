package catalog

import (
	"errors"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/core"
)

func mergeTestProfile() core.Profile {
	return core.Profile{
		Name: "small",
		Addons: []core.AddonRef{
			{
				Name: "cert-manager", Chart: "cert-manager",
				Repository: "https://charts.jetstack.io", Version: "1.15.3", Namespace: "cert-manager",
				Values: map[string]any{"replicaCount": 1},
			},
			{
				Name: "external-dns", Chart: "external-dns",
				Repository: "https://kubernetes-sigs.github.io/external-dns", Version: "1.14.0", Namespace: "external-dns",
			},
		},
	}
}

func TestMerge_NoOverrides_ReturnsProfileUnchanged(t *testing.T) {
	profile := mergeTestProfile()

	merged, err := Merge(profile, nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(merged.Addons) != len(profile.Addons) {
		t.Fatalf("addon count changed: %d -> %d", len(profile.Addons), len(merged.Addons))
	}
}

func TestMerge_VersionOverride(t *testing.T) {
	profile := mergeTestProfile()

	merged, err := Merge(profile, []core.AddonOverride{{Name: "cert-manager", Version: "1.16.0"}})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	addon := findAddon(t, merged, "cert-manager")
	if addon.Version != "1.16.0" {
		t.Errorf("version = %q, want 1.16.0", addon.Version)
	}

	// The base profile passed in must not be mutated: two clusters resolving
	// the same profile must not see each other's overrides.
	if profile.Addons[0].Version != "1.15.3" {
		t.Errorf("base profile was mutated: version = %q", profile.Addons[0].Version)
	}

	// Every other addon, and every other field of the overridden one, is
	// untouched — the whole point of a patch is that it says only what changes.
	other := findAddon(t, merged, "external-dns")
	if other.Version != "1.14.0" {
		t.Errorf("unrelated addon changed: version = %q", other.Version)
	}
	if addon.Chart != "cert-manager" || addon.Namespace != "cert-manager" {
		t.Errorf("unrelated fields changed: %+v", addon)
	}
}

func TestMerge_ValuesOverride_OverlaysRatherThanReplaces(t *testing.T) {
	profile := mergeTestProfile()

	merged, err := Merge(profile, []core.AddonOverride{
		{Name: "cert-manager", Values: map[string]any{"resources": map[string]any{"limits": "100m"}}},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	addon := findAddon(t, merged, "cert-manager")
	if addon.Values["replicaCount"] != 1 {
		t.Errorf("base value was dropped: %+v", addon.Values)
	}
	if _, ok := addon.Values["resources"]; !ok {
		t.Errorf("override value was not applied: %+v", addon.Values)
	}
}

func TestMerge_Disable_RemovesTheAddon(t *testing.T) {
	profile := mergeTestProfile()

	merged, err := Merge(profile, []core.AddonOverride{{Name: "external-dns", Disable: true}})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	if len(merged.Addons) != 1 {
		t.Fatalf("addons = %+v, want exactly cert-manager left", merged.Addons)
	}
	if merged.Addons[0].Name != "cert-manager" {
		t.Errorf("wrong addon survived: %+v", merged.Addons)
	}
}

func TestMerge_UnknownAddon_Errors(t *testing.T) {
	profile := mergeTestProfile()

	_, err := Merge(profile, []core.AddonOverride{{Name: "does-not-exist", Version: "1.0.0"}})
	if !errors.Is(err, ErrUnknownOverride) {
		t.Errorf("error = %v, want one wrapping ErrUnknownOverride", err)
	}
}

func TestMerge_DoesNotDuplicateAddons(t *testing.T) {
	profile := mergeTestProfile()

	merged, err := Merge(profile, []core.AddonOverride{{Name: "cert-manager", Version: "1.16.0"}})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(merged.Addons) != len(profile.Addons) {
		t.Errorf("addon count = %d, want %d (a patch must not add entries)", len(merged.Addons), len(profile.Addons))
	}
}

func findAddon(t *testing.T, profile core.Profile, name string) core.AddonRef {
	t.Helper()
	for _, a := range profile.Addons {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("addon %q not found in %+v", name, profile.Addons)
	return core.AddonRef{}
}
