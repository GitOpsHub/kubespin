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
		"cert-manager", "gateway-api", "external-secrets",
		"kyverno", "kyverno-policies", "cluster-autoscaler", "argocd",
		"kube-prometheus-stack", "fluent-bit", "opencost", "external-dns",
		"ingress-nginx",
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

	for _, want := range []string{"otel-collector", "argocd", "velero", "falco"} {
		if !large[want] {
			t.Errorf("size large is missing %s", want)
		}
	}
}

// large raises Pod Security from baseline to restricted, and every size
// audits rather than enforces: restricted forbids the host access
// node-exporter, falco and fluent-bit need, so enforcing it would refuse the
// size's own addons.
func TestSizeLarge_StrictPolicySetReplacesBaseline(t *testing.T) {
	for size, want := range map[*core.Profile]string{&sizeSmall: "baseline", &sizeMedium: "baseline", &sizeLarge: "restricted"} {
		policies, ok := size.Addon("kyverno-policies")
		if !ok {
			t.Fatalf("size %s has no kyverno-policies addon", size.Name)
		}
		if got := policies.Values["podSecurityStandard"]; got != want {
			t.Errorf("size %s: podSecurityStandard = %v, want %s", size.Name, got, want)
		}
		if got := policies.Values["validationFailureAction"]; got != "Audit" {
			t.Errorf("size %s: validationFailureAction = %v, want Audit", size.Name, got)
		}
	}
}

// Every size ships Argo CD and exactly one cluster-autoscaler per cloud —
// the catalog carries an AWS-configured entry and a GCP/Azure one under the
// same name, so two copies must never survive ForProvider together — and
// Karpenter on no cloud.
func TestEverySize_ArgoCDAndAutoscalerPerProvider(t *testing.T) {
	for _, size := range []core.Profile{sizeSmall, sizeMedium, sizeLarge} {
		t.Run(size.Name, func(t *testing.T) {
			for _, provider := range core.Providers() {
				resolved := size.ForProvider(provider)
				names := addonNames(resolved)

				if !names["argocd"] {
					t.Errorf("%s/%s: missing argocd", size.Name, provider)
				}

				if names["karpenter"] {
					t.Errorf("%s/%s: carries karpenter", size.Name, provider)
				}
				var autoscalers []core.AddonRef
				for _, a := range resolved.Addons {
					if a.Name == "cluster-autoscaler" {
						autoscalers = append(autoscalers, a)
					}
				}
				if len(autoscalers) != 1 {
					t.Fatalf("%s/%s: %d cluster-autoscaler entries, want exactly 1", size.Name, provider, len(autoscalers))
				}
				isAWSConfigured := autoscalers[0].Values["cloudProvider"] == "aws"
				if isAWSConfigured != (provider == core.ProviderAWS) {
					t.Errorf("%s/%s: cluster-autoscaler cloudProvider = %v", size.Name, provider, autoscalers[0].Values["cloudProvider"])
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
