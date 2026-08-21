package catalog

import (
	"context"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/core"
)

func TestBuiltinSizes_AreValid(t *testing.T) {
	for _, size := range []core.Profile{sizeSmall, sizeMedium, sizeLarge} {
		t.Run(size.Name, func(t *testing.T) {
			if err := size.Validate(); err != nil {
				t.Errorf("%s is invalid: %v", size.Name, err)
			}
		})
	}
}

func TestBuiltinResolver_ResolvesEverySize(t *testing.T) {
	r := NewBuiltinResolver()
	for _, size := range core.Sizes() {
		t.Run(string(size), func(t *testing.T) {
			profile, err := r.Resolve(context.Background(), size)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if profile.Name != string(size) {
				t.Errorf("Name = %s, want %s", profile.Name, size)
			}
		})
	}
}

func TestSizeSmall_CarriesTheFullNamedAddonSet(t *testing.T) {
	small := addonNames(sizeSmall)
	for _, want := range []string{
		"cilium", "cert-manager", "gateway-api", "external-secrets",
		"kyverno", "kyverno-policies", "cluster-autoscaler", "karpenter", "argocd",
		"kube-prometheus-stack", "fluent-bit", "opencost", "external-dns",
		"ingress-nginx", "fleet-status-reporter",
	} {
		if !small[want] {
			t.Errorf("size small is missing %s", want)
		}
	}
}

func TestSizeMedium_IsASupersetOfSizeSmall(t *testing.T) {
	small := addonNames(sizeSmall)
	medium := addonNames(sizeMedium)

	for name := range small {
		if !medium[name] {
			t.Errorf("size medium is missing small's %s addon", name)
		}
	}
	for _, want := range []string{"velero", "falco"} {
		if !medium[want] {
			t.Errorf("size medium is missing %s", want)
		}
	}
}

func TestSizeLarge_ReplacesBaselinePolicyRatherThanDuplicatingIt(t *testing.T) {
	large := addonNames(sizeLarge)

	count := 0
	for _, a := range sizeLarge.Addons {
		if a.Name == "kyverno-policies" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("kyverno-policies appears %d times, want exactly 1", count)
	}

	for _, want := range []string{"audit-logging", "otel-collector", "argocd", "velero", "falco"} {
		if !large[want] {
			t.Errorf("size large is missing %s", want)
		}
	}
}

func TestSizeLarge_StrictPolicySetReplacesBaseline(t *testing.T) {
	for _, a := range sizeLarge.Addons {
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
	t.Fatal("size large has no kyverno-policies addon")
}

// Every size ships Argo CD and exactly one cloud-appropriate autoscaler —
// Karpenter on AWS (EKS-only technology), cluster-autoscaler on GCP/Azure —
// never both, so the two never compete over the same nodes.
func TestEverySize_ArgoCDAndAutoscalerPerProvider(t *testing.T) {
	for _, size := range []core.Profile{sizeSmall, sizeMedium, sizeLarge} {
		t.Run(size.Name, func(t *testing.T) {
			for _, provider := range core.Providers() {
				resolved := size.ForProvider(provider)
				names := addonNames(resolved)

				if !names["argocd"] {
					t.Errorf("%s/%s: missing argocd", size.Name, provider)
				}

				hasKarpenter := names["karpenter"]
				hasClusterAutoscaler := names["cluster-autoscaler"]
				switch provider {
				case core.ProviderAWS:
					if !hasKarpenter || hasClusterAutoscaler {
						t.Errorf("%s/%s: karpenter=%v cluster-autoscaler=%v, want karpenter only",
							size.Name, provider, hasKarpenter, hasClusterAutoscaler)
					}
				case core.ProviderGCP, core.ProviderAzure:
					if hasKarpenter || !hasClusterAutoscaler {
						t.Errorf("%s/%s: karpenter=%v cluster-autoscaler=%v, want cluster-autoscaler only",
							size.Name, provider, hasKarpenter, hasClusterAutoscaler)
					}
				}
			}
		})
	}
}

func addonNames(profile core.Profile) map[string]bool {
	out := make(map[string]bool, len(profile.Addons))
	for _, a := range profile.Addons {
		out[a.Name] = true
	}
	return out
}
