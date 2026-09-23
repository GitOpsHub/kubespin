package catalog

import (
	"context"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/argocd"
	"github.com/GitOpsHub/kubespin/internal/core"
)

func testClusterSpec(provider core.Provider) core.ClusterSpec {
	return core.ClusterSpec{
		ID:       "team-payments-prod",
		Provider: provider,
		Region:   "us-east-1",
		Access:   core.AccessPrivate,
		Size:     core.SizeSmall,
		Subnets:  []string{"subnet-aaa", "subnet-bbb"},
	}
}

func TestResolveForCluster_Autopilot_OnlyArgoCD(t *testing.T) {
	for _, provider := range []core.Provider{core.ProviderGCP, core.ProviderAWS} {
		t.Run(string(provider), func(t *testing.T) {
			spec := testClusterSpec(provider)
			spec.Autopilot = true

			profile, err := ResolveForCluster(context.Background(), NewBuiltinResolver(), spec)
			if err != nil {
				t.Fatalf("ResolveForCluster: %v", err)
			}
			if len(profile.Addons) != 1 || profile.Addons[0].Name != argocd.ReleaseName {
				t.Fatalf("expected only argocd under Autopilot, got %+v", profile.Addons)
			}
		})
	}
}

func TestResolveForCluster_NonAutopilot_KeepsAutoscalers(t *testing.T) {
	gcpProfile, err := ResolveForCluster(context.Background(), NewBuiltinResolver(), testClusterSpec(core.ProviderGCP))
	if err != nil {
		t.Fatalf("ResolveForCluster: %v", err)
	}
	if _, ok := gcpProfile.Addon("cluster-autoscaler"); !ok {
		t.Error("expected cluster-autoscaler to remain without Autopilot")
	}

	awsProfile, err := ResolveForCluster(context.Background(), NewBuiltinResolver(), testClusterSpec(core.ProviderAWS))
	if err != nil {
		t.Fatalf("ResolveForCluster: %v", err)
	}
	if _, ok := awsProfile.Addon("cluster-autoscaler"); !ok {
		t.Error("expected cluster-autoscaler to remain without Autopilot")
	}
}

// The AWS autoscaler's values name its own cluster and region, which only
// ResolveForCluster knows; no placeholder may reach the rendered values, and
// the shared catalog entry must stay untouched for the next cluster.
func TestResolveForCluster_FillsClusterValuesIntoTheAWSAutoscaler(t *testing.T) {
	spec := testClusterSpec(core.ProviderAWS)
	profile, err := ResolveForCluster(context.Background(), NewBuiltinResolver(), spec)
	if err != nil {
		t.Fatalf("ResolveForCluster: %v", err)
	}
	autoscaler, ok := profile.Addon("cluster-autoscaler")
	if !ok {
		t.Fatal("expected cluster-autoscaler on aws")
	}
	if got := autoscaler.Values["awsRegion"]; got != spec.Region {
		t.Errorf("awsRegion = %v, want %s", got, spec.Region)
	}
	discovery, _ := autoscaler.Values["autoDiscovery"].(map[string]any)
	if got := discovery["clusterName"]; got != spec.ID.String() {
		t.Errorf("autoDiscovery.clusterName = %v, want %s", got, spec.ID)
	}

	for _, a := range baseAddons {
		if a.Name == "cluster-autoscaler" && a.SupportsProvider(core.ProviderAWS) && a.Values["awsRegion"] != RegionPlaceholder {
			t.Errorf("catalog entry was mutated: awsRegion = %v", a.Values["awsRegion"])
		}
	}
}

// OpenCost is served from a LoadBalancer that follows the cluster's access
// mode, and reads kube-prometheus-stack's Prometheus.
func TestResolveForCluster_OpenCostLoadBalancerFollowsAccessMode(t *testing.T) {
	spec := testClusterSpec(core.ProviderAWS)
	spec.Access = core.AccessPublic
	spec.AuthorizedCIDRs = []string{"198.51.100.7/32"}

	profile, err := ResolveForCluster(context.Background(), NewBuiltinResolver(), spec)
	if err != nil {
		t.Fatalf("ResolveForCluster: %v", err)
	}
	opencost, ok := profile.Addon("opencost")
	if !ok {
		t.Fatal("expected opencost in the profile")
	}
	service, _ := opencost.Values["service"].(map[string]any)
	if service["type"] != "LoadBalancer" {
		t.Errorf("service.type = %v, want LoadBalancer", service["type"])
	}
	if ranges, _ := service["loadBalancerSourceRanges"].([]any); len(ranges) != 1 || ranges[0] != "198.51.100.7/32" {
		t.Errorf("loadBalancerSourceRanges = %v, want the cluster's authorized CIDRs", service["loadBalancerSourceRanges"])
	}

	cfg, _ := opencost.Values["opencost"].(map[string]any)
	prom, _ := cfg["prometheus"].(map[string]any)
	internal, _ := prom["internal"].(map[string]any)
	if internal["serviceName"] != "kube-prometheus-stack-prometheus" || internal["namespaceName"] != "monitoring" {
		t.Errorf("prometheus.internal = %v, want kube-prometheus-stack's Prometheus", internal)
	}
}
