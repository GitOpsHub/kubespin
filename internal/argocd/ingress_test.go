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
		Name: "small",
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

func loadBalancerAddon(exposure string) core.AddonRef {
	return core.AddonRef{
		Name: "web-ui", Chart: "web-ui", Repository: "https://example", Version: "1.0.0", Namespace: "web-ui",
		Values: map[string]any{
			"ingress": map[string]any{"exposure": exposure},
			"service": map[string]any{"type": "LoadBalancer", "annotations": map[string]any{"keep": "me"}},
		},
	}
}

// A private cluster's LoadBalancer Service must come out internal on
// whichever cloud renders it, whatever the addon asked for.
func TestApplyIngressDefaults_PrivateLoadBalancerGetsInternalAnnotations(t *testing.T) {
	patched := ApplyIngressDefaults(core.AccessPrivate, loadBalancerAddon("external"),
		WithAuthorizedCIDRs([]string{"203.0.113.0/24"}))

	service, _ := patched.Values["service"].(map[string]any)
	annotations, _ := service["annotations"].(map[string]any)
	for k, v := range internalLoadBalancerAnnotations {
		if annotations[k] != v {
			t.Errorf("annotation %s = %v, want %v", k, annotations[k], v)
		}
	}
	if annotations["keep"] != "me" {
		t.Error("an existing service annotation was dropped")
	}
	if _, ok := service["loadBalancerSourceRanges"]; ok {
		t.Error("an internal load balancer was given source ranges meant for an external one")
	}
}

// A public cluster honours an external request, but only admits the
// cluster's authorized CIDRs.
func TestApplyIngressDefaults_PublicLoadBalancerIsLimitedToAuthorizedCIDRs(t *testing.T) {
	addon := loadBalancerAddon("external")
	patched := ApplyIngressDefaults(core.AccessPublic, addon, WithAuthorizedCIDRs([]string{"203.0.113.0/24"}))

	service, _ := patched.Values["service"].(map[string]any)
	ranges, _ := service["loadBalancerSourceRanges"].([]any)
	if len(ranges) != 1 || ranges[0] != "203.0.113.0/24" {
		t.Errorf("loadBalancerSourceRanges = %v, want [203.0.113.0/24]", service["loadBalancerSourceRanges"])
	}
	annotations, _ := service["annotations"].(map[string]any)
	if _, ok := annotations["service.beta.kubernetes.io/aws-load-balancer-internal"]; ok {
		t.Error("an external load balancer was annotated internal")
	}
	original, _ := addon.Values["service"].(map[string]any)
	if _, ok := original["loadBalancerSourceRanges"]; ok {
		t.Error("ApplyIngressDefaults mutated the caller's service values")
	}
}

// A public cluster with no authorized CIDRs leaves the load balancer open,
// the same as its API endpoint.
func TestApplyIngressDefaults_PublicLoadBalancerWithoutCIDRsHasNoSourceRanges(t *testing.T) {
	patched := ApplyIngressDefaults(core.AccessPublic, loadBalancerAddon("external"))
	service, _ := patched.Values["service"].(map[string]any)
	if _, ok := service["loadBalancerSourceRanges"]; ok {
		t.Error("source ranges were set with no authorized CIDRs to take them from")
	}
}

// argocd-server (server.service) and ingress-nginx's controller
// (controller.service) stay internet-facing on every cluster, private
// included, with no source-range limit: operator UIs depend on reaching them.
// Access-mode templating must leave both Services exactly as declared.
func TestApplyProfileIngressDefaults_ArgoCDAndIngressNginxStayPublic(t *testing.T) {
	for _, access := range []core.Access{core.AccessPrivate, core.AccessPublic} {
		profile := core.Profile{Name: "small", Addons: []core.AddonRef{
			DefaultAddon,
			{
				Name: "ingress-nginx", Chart: "ingress-nginx",
				Values: map[string]any{"ingress": map[string]any{"exposure": "internal"}},
			},
		}}

		patched := ApplyProfileIngressDefaults(access, profile, WithAuthorizedCIDRs([]string{"203.0.113.0/24"}))

		argo := patched.Addons[0].Values
		if _, ok := argo["ingress"]; ok {
			t.Errorf("%s: argo-cd gained a top-level ingress block its chart does not read", access)
		}
		server, _ := argo["server"].(map[string]any)
		service, _ := server["service"].(map[string]any)
		if len(service) != 1 || service["type"] != "LoadBalancer" {
			t.Errorf("%s: server.service = %v, want only type LoadBalancer", access, service)
		}
		if _, ok := patched.Addons[1].Values["controller"]; ok {
			t.Errorf("%s: ingress-nginx controller values were patched: %v", access, patched.Addons[1].Values["controller"])
		}
	}
}
