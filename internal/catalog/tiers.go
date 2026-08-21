package catalog

import "github.com/GitOpsHub/kubespin/internal/core"

// withAddons returns a copy of base's addon list plus extra, without
// aliasing base's backing array — appending to a size's Addons directly
// would risk one size's growth silently overwriting another's slice if their
// capacities ever happened to overlap.
func withAddons(base []core.AddonRef, extra ...core.AddonRef) []core.AddonRef {
	out := make([]core.AddonRef, 0, len(base)+len(extra))
	out = append(out, base...)
	out = append(out, extra...)
	return out
}

// sizeMedium is sizeSmall's set plus Velero and Falco.
var sizeMedium = core.Profile{
	Name: "medium",
	Addons: withAddons(baseAddons,
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
	),
}

// sizeLarge is sizeMedium's set, with the baseline Kyverno policy addon
// replaced by a strict compliance-oriented set, plus audit logging and OTel
// tracing.
//
// It replaces kyverno-policies rather than adding a second Kyverno addon:
// two Argo CD Applications installing overlapping ClusterPolicy resources
// into the same cluster would fight over ownership, so the strict set
// supersedes the baseline one instead of layering on top of it.
var sizeLarge = core.Profile{
	Name: "large",
	Addons: withAddons(replaceAddon(sizeMedium.Addons, "kyverno-policies", core.AddonRef{
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
