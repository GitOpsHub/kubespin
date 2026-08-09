package catalog

import "github.com/GitOpsHub/kubespin/internal/core"

// withAddons returns a copy of base's addon list plus extra, without
// aliasing base's backing array — appending to tierSmall.Addons directly
// would risk one tier's growth silently overwriting another's slice if their
// capacities ever happened to overlap.
func withAddons(base []core.AddonRef, extra ...core.AddonRef) []core.AddonRef {
	out := make([]core.AddonRef, 0, len(base)+len(extra))
	out = append(out, base...)
	out = append(out, extra...)
	return out
}

// tierStandard is a placeholder for the real tier-standard catalog entry:
// tier-small's set plus Velero, Falco, Karpenter, and Argo CD tracked as a
// catalog entry in its own right.
//
// Argo CD is installed directly (M5's Helm-as-library install, not through
// app-of-apps — it has to exist before app-of-apps can sync anything into
// it), but from this tier up it is also tracked here so `fleet audit` and
// `fleet update` can see and pin its version like any other addon.
//
// Karpenter is EKS-specific; this catalog has no per-provider addon
// filtering yet (core.AddonRef carries no provider constraint), so it is
// listed unconditionally here. The real platform-profiles repo (M4's still-
// open item) will need that filtering before this tier is provider-safe.
var tierStandard = core.Profile{
	Name:    "tier-standard",
	Version: "1.0.0",
	Addons: withAddons(tierSmall.Addons,
		core.AddonRef{
			Name:       "argocd",
			Chart:      "argo-cd",
			Repository: "https://argoproj.github.io/argo-helm",
			Version:    "7.7.11",
			Namespace:  "argocd",
		},
		core.AddonRef{
			Name:       "velero",
			Chart:      "velero",
			Repository: "https://vmware-tanzu.github.io/helm-charts",
			Version:    "8.1.0",
			Namespace:  "velero",
		},
		core.AddonRef{
			Name:       "falco",
			Chart:      "falco",
			Repository: "https://falcosecurity.github.io/charts",
			Version:    "4.9.0",
			Namespace:  "falco",
		},
		core.AddonRef{
			Name:       "karpenter",
			Chart:      "karpenter",
			Repository: "oci://public.ecr.aws/karpenter/karpenter",
			Version:    "1.0.6",
			Namespace:  "karpenter",
			// EKS-only; see the package-level note above.
			Values: map[string]any{"providerOnly": "aws"},
		},
	),
}

// tierRegulated is a placeholder for the real tier-regulated catalog entry:
// tier-standard's set, with the baseline Kyverno policy addon replaced by a
// strict compliance-oriented set, plus audit logging and OTel tracing.
//
// It replaces kyverno-policies rather than adding a second Kyverno addon:
// two Argo CD Applications installing overlapping ClusterPolicy resources
// into the same cluster would fight over ownership, so the strict set
// supersedes the baseline one instead of layering on top of it.
var tierRegulated = core.Profile{
	Name:    "tier-regulated",
	Version: "1.0.0",
	Addons: withAddons(replaceAddon(tierStandard.Addons, "kyverno-policies", core.AddonRef{
		Name:       "kyverno-policies",
		Chart:      "kyverno-policies-regulated",
		Repository: "https://charts.kubespin.dev",
		Version:    "0.1.0",
		Namespace:  "kyverno",
		Values: map[string]any{"policies": map[string]any{
			"publicExposureDeny":     true,
			"denyPrivilegedPods":     true,
			"mandatoryQuotas":        true,
			"mandatoryNetworkPolicy": true,
			"requireImageSignature":  true,
		}},
	}),
		core.AddonRef{
			Name:       "audit-logging",
			Chart:      "audit-logging",
			Repository: "https://charts.kubespin.dev",
			Version:    "0.1.0",
			Namespace:  "kubespin-system",
		},
		core.AddonRef{
			Name:       "otel-collector",
			Chart:      "opentelemetry-collector",
			Repository: "https://open-telemetry.github.io/opentelemetry-helm-charts",
			Version:    "0.108.0",
			Namespace:  "observability",
		},
	),
}

// replaceAddon returns a copy of addons with the entry named name replaced
// by replacement, without aliasing addons' backing array.
func replaceAddon(addons []core.AddonRef, name string, replacement core.AddonRef) []core.AddonRef {
	out := make([]core.AddonRef, len(addons))
	for i, a := range addons {
		if a.Name == name {
			out[i] = replacement
			continue
		}
		out[i] = a
	}
	return out
}
