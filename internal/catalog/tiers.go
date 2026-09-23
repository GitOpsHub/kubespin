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
			// 12.x: earlier charts ran their CRD-upgrade job on
			// bitnami/kubectl, which Bitnami has pulled from Docker Hub.
			Name:       "velero",
			Chart:      "velero",
			Repository: "https://vmware-tanzu.github.io/helm-charts",
			Version:    "12.2.0",
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

// sizeLarge is sizeMedium's set, with the Pod Security policies raised from
// baseline to restricted, plus an OpenTelemetry collector.
//
// It replaces kyverno-policies rather than adding a second Kyverno addon:
// two Argo CD Applications installing overlapping policies into the same
// cluster would fight over ownership, so the restricted set supersedes the
// baseline one instead of layering on top of it. The restricted set stays in
// Audit mode: node-exporter, falco and fluent-bit all need host access that
// restricted forbids, so enforcing it would refuse the size's own addons.
var sizeLarge = core.Profile{
	Name: "large",
	Addons: withAddons(replaceAddon(sizeMedium.Addons, "kyverno-policies", core.AddonRef{
		Name:       "kyverno-policies",
		Chart:      "kyverno-policies",
		Repository: "https://kyverno.github.io/kyverno",
		Version:    "3.9.1",
		Namespace:  "kyverno",
		Values: map[string]any{
			"podSecurityStandard":     "restricted",
			"validationFailureAction": "Audit",
		},
	}),
		core.AddonRef{
			// The chart renders nothing without a mode and an image: it
			// ships no default for either.
			Name:       "otel-collector",
			Chart:      "opentelemetry-collector",
			Repository: "https://open-telemetry.github.io/opentelemetry-helm-charts",
			Version:    "0.108.0",
			Namespace:  "observability",
			Values: map[string]any{
				"mode":  "deployment",
				"image": map[string]any{"repository": "otel/opentelemetry-collector-k8s"},
			},
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
