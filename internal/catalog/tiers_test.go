package catalog

import (
	"context"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/core"
)

func TestBuiltinTiers_AreValid(t *testing.T) {
	for _, tier := range []core.Profile{tierSmall, tierStandard, tierRegulated} {
		t.Run(tier.Name, func(t *testing.T) {
			if err := tier.Validate(); err != nil {
				t.Errorf("%s is invalid: %v", tier.Name, err)
			}
		})
	}
}

func TestBuiltinResolver_ResolvesEveryTier(t *testing.T) {
	r := NewBuiltinResolver()
	for _, ref := range []core.ProfileRef{
		{Name: "tier-small", Version: "1.0.0"},
		{Name: "tier-standard", Version: "1.0.0"},
		{Name: "tier-regulated", Version: "1.0.0"},
	} {
		t.Run(ref.Name, func(t *testing.T) {
			profile, err := r.Resolve(context.Background(), ref)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if profile.Ref() != ref {
				t.Errorf("Ref() = %s, want %s", profile.Ref(), ref)
			}
		})
	}
}

func TestTierStandard_IsASupersetOfTierSmall(t *testing.T) {
	small := addonNames(tierSmall)
	standard := addonNames(tierStandard)

	for name := range small {
		if !standard[name] {
			t.Errorf("tier-standard is missing tier-small's %s addon", name)
		}
	}
	for _, want := range []string{"argocd", "velero", "falco", "karpenter"} {
		if !standard[want] {
			t.Errorf("tier-standard is missing %s", want)
		}
	}
}

func TestTierRegulated_ReplacesBaselinePolicyRatherThanDuplicatingIt(t *testing.T) {
	regulated := addonNames(tierRegulated)

	count := 0
	for _, a := range tierRegulated.Addons {
		if a.Name == "kyverno-policies" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("kyverno-policies appears %d times, want exactly 1", count)
	}

	for _, want := range []string{"audit-logging", "otel-collector", "argocd", "velero", "falco"} {
		if !regulated[want] {
			t.Errorf("tier-regulated is missing %s", want)
		}
	}
}

func TestTierRegulated_StrictPolicySetReplacesBaseline(t *testing.T) {
	for _, a := range tierRegulated.Addons {
		if a.Name != "kyverno-policies" {
			continue
		}
		policies, ok := a.Values["policies"].(map[string]any)
		if !ok {
			t.Fatalf("kyverno-policies has no policies values: %+v", a.Values)
		}
		for _, rule := range []string{
			"publicExposureDeny", "denyPrivilegedPods", "mandatoryQuotas",
			"mandatoryNetworkPolicy", "requireImageSignature",
		} {
			if policies[rule] != true {
				t.Errorf("policy %s = %v, want true", rule, policies[rule])
			}
		}
		return
	}
	t.Fatal("tier-regulated has no kyverno-policies addon")
}

func addonNames(profile core.Profile) map[string]bool {
	out := make(map[string]bool, len(profile.Addons))
	for _, a := range profile.Addons {
		out[a.Name] = true
	}
	return out
}
