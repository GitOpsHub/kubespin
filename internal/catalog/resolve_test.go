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
	if _, ok := awsProfile.Addon("karpenter"); !ok {
		t.Error("expected karpenter to remain without Autopilot")
	}
}
