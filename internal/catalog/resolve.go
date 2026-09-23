package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/GitOpsHub/kubespin/internal/argocd"
	"github.com/GitOpsHub/kubespin/internal/core"
)

// ResolveForCluster resolves spec's profile, applies its per-cluster override
// patch, and templates ingress/Gateway addons for spec's access mode, so
// every caller renders the same resolved addon set for a given cluster rather
// than each reimplementing the resolve-merge-template sequence and risking the
// two diverging.
func ResolveForCluster(ctx context.Context, resolver Resolver, spec core.ClusterSpec) (core.Profile, error) {
	profile, err := resolver.Resolve(ctx, spec.Size)
	if err != nil {
		return core.Profile{}, fmt.Errorf("resolving size %s for %s: %w", spec.Size, spec.ID, err)
	}
	profile = profile.ForProvider(spec.Provider)
	profile = profile.ForAutopilot(spec.Autopilot)
	profile = withArgoCDAddon(profile)
	profile = withClusterValues(profile, spec)

	merged, err := Merge(profile, spec.Overrides)
	if err != nil {
		return core.Profile{}, fmt.Errorf("applying overrides for %s: %w", spec.ID, err)
	}

	return argocd.ApplyProfileIngressDefaults(spec.Access, merged, argocd.WithAuthorizedCIDRs(spec.AuthorizedCIDRs)), nil
}

// withArgoCDAddon ensures profile always carries an "argocd" catalog entry,
// defaulting to argocd.DefaultAddon on the rare chance a size's catalog entry
// doesn't (every builtin size does, via baseAddons). Argo CD is installed on
// every cluster regardless of whether the catalog tracks it, so without this
// a cluster.yaml override naming "argocd" on a profile that doesn't carry it
// would fail Merge with ErrUnknownOverride even though the addon is always
// installed.
func withArgoCDAddon(profile core.Profile) core.Profile {
	if _, ok := profile.Addon(argocd.ReleaseName); ok {
		return profile
	}
	profile.Addons = append(append([]core.AddonRef(nil), profile.Addons...), argocd.DefaultAddon)
	return profile
}

// Placeholders a catalog addon's values may use for facts only known per
// cluster. ResolveForCluster substitutes them before override patches apply,
// so an override can still replace the substituted value outright.
const (
	ClusterIDPlaceholder = "${CLUSTER_ID}"
	RegionPlaceholder    = "${REGION}"
)

// withClusterValues returns profile with every placeholder in its addons'
// values replaced by spec's own values. Values are copied, never mutated:
// the catalog's maps are shared by every size and every cluster resolved
// from them.
func withClusterValues(profile core.Profile, spec core.ClusterSpec) core.Profile {
	replacer := strings.NewReplacer(
		ClusterIDPlaceholder, spec.ID.String(),
		RegionPlaceholder, spec.Region,
	)
	out := profile
	out.Addons = make([]core.AddonRef, len(profile.Addons))
	for i, addon := range profile.Addons {
		if addon.Values != nil {
			// substitute returns a map for a map, so the assertion always holds.
			values, _ := substitute(addon.Values, replacer).(map[string]any)
			addon.Values = values
		}
		out.Addons[i] = addon
	}
	return out
}

func substitute(v any, replacer *strings.Replacer) any {
	switch v := v.(type) {
	case string:
		return replacer.Replace(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = substitute(item, replacer)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = substitute(item, replacer)
		}
		return out
	default:
		return v
	}
}
