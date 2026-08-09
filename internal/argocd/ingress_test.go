package argocd

import (
	"testing"

	"github.com/GitOpsHub/kubespin/internal/core"
)

func TestResolveExposure(t *testing.T) {
	tests := []struct {
		name      string
		access    core.Access
		requested Exposure
		want      Exposure
	}{
		{"private cluster, addon wants external -> internal wins", core.AccessPrivate, ExposureExternal, ExposureInternal},
		{"private cluster, addon wants internal -> internal", core.AccessPrivate, ExposureInternal, ExposureInternal},
		{"public cluster, addon wants external -> external", core.AccessPublic, ExposureExternal, ExposureExternal},
		{"public cluster, addon wants internal -> internal", core.AccessPublic, ExposureInternal, ExposureInternal},
		{"public cluster, addon unset -> internal", core.AccessPublic, "", ExposureInternal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveExposure(tc.access, tc.requested); got != tc.want {
				t.Errorf("ResolveExposure(%s, %s) = %s, want %s", tc.access, tc.requested, got, tc.want)
			}
		})
	}
}

func TestApplyIngressDefaults_PrivateClusterForcesInternal(t *testing.T) {
	addon := core.AddonRef{
		Name: "ingress-nginx", Values: map[string]any{"ingress": map[string]any{"exposure": "external"}},
	}

	patched := ApplyIngressDefaults(core.AccessPrivate, addon)

	ingress, ok := patched.Values["ingress"].(map[string]any)
	if !ok {
		t.Fatalf("ingress value is not a map: %+v", patched.Values["ingress"])
	}
	if ingress["exposure"] != string(ExposureInternal) {
		t.Errorf("exposure = %v, want internal", ingress["exposure"])
	}
	if ingress["internal"] != true {
		t.Errorf("internal = %v, want true", ingress["internal"])
	}
}

func TestApplyIngressDefaults_PublicClusterHonoursExternalRequest(t *testing.T) {
	addon := core.AddonRef{
		Name: "ingress-nginx", Values: map[string]any{"ingress": map[string]any{"exposure": "external"}},
	}

	patched := ApplyIngressDefaults(core.AccessPublic, addon)

	ingress, ok := patched.Values["ingress"].(map[string]any)
	if !ok {
		t.Fatalf("ingress value is not a map: %+v", patched.Values["ingress"])
	}
	if ingress["exposure"] != string(ExposureExternal) {
		t.Errorf("exposure = %v, want external", ingress["exposure"])
	}
	if ingress["internal"] != false {
		t.Errorf("internal = %v, want false", ingress["internal"])
	}
}

func TestApplyIngressDefaults_NoIngressValues_DefaultsToInternal(t *testing.T) {
	addon := core.AddonRef{Name: "ingress-nginx"}

	patched := ApplyIngressDefaults(core.AccessPublic, addon)

	ingress, ok := patched.Values["ingress"].(map[string]any)
	if !ok {
		t.Fatalf("ingress value is not a map: %+v", patched.Values["ingress"])
	}
	if ingress["exposure"] != string(ExposureInternal) {
		t.Errorf("exposure = %v, want internal", ingress["exposure"])
	}
}

func TestApplyIngressDefaults_PreservesOtherValues(t *testing.T) {
	addon := core.AddonRef{
		Name: "ingress-nginx",
		Values: map[string]any{
			"replicaCount": 2,
			"ingress":      map[string]any{"className": "nginx", "exposure": "external"},
		},
	}

	patched := ApplyIngressDefaults(core.AccessPublic, addon)

	if patched.Values["replicaCount"] != 2 {
		t.Errorf("replicaCount = %v, want it preserved", patched.Values["replicaCount"])
	}
	ingress, ok := patched.Values["ingress"].(map[string]any)
	if !ok {
		t.Fatalf("ingress value is not a map: %+v", patched.Values["ingress"])
	}
	if ingress["className"] != "nginx" {
		t.Errorf("className = %v, want it preserved", ingress["className"])
	}
}

func TestApplyProfileIngressDefaults_OnlyPatchesIngressAddons(t *testing.T) {
	profile := core.Profile{
		Name: "tier-small", Version: "1.0.0",
		Addons: []core.AddonRef{
			{Name: "ingress-nginx", Values: map[string]any{"ingress": map[string]any{"exposure": "external"}}},
			{Name: "cert-manager"},
		},
	}

	patched := ApplyProfileIngressDefaults(core.AccessPrivate, profile)

	ingress, ok := patched.Addons[0].Values["ingress"].(map[string]any)
	if !ok {
		t.Fatalf("ingress value is not a map: %+v", patched.Addons[0].Values["ingress"])
	}
	if ingress["exposure"] != string(ExposureInternal) {
		t.Errorf("exposure = %v, want internal", ingress["exposure"])
	}
	if patched.Addons[1].Values != nil {
		t.Errorf("cert-manager gained values it never had: %+v", patched.Addons[1].Values)
	}
}

func TestApplyIngressDefaults_DoesNotMutateInput(t *testing.T) {
	original := map[string]any{"ingress": map[string]any{"exposure": "external"}}
	addon := core.AddonRef{Name: "ingress-nginx", Values: original}

	ApplyIngressDefaults(core.AccessPrivate, addon)

	ingress, ok := original["ingress"].(map[string]any)
	if !ok || ingress["exposure"] != "external" {
		t.Error("ApplyIngressDefaults mutated the caller's values map")
	}
}
