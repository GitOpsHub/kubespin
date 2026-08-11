package catalog

import (
	"context"
	"fmt"

	"github.com/GitOpsHub/kubespin/internal/argocd"
	"github.com/GitOpsHub/kubespin/internal/core"
)

// ResolveForCluster resolves spec's profile, applies its per-cluster override
// patch, and templates ingress/Gateway addons for spec's access mode, so
// every caller — the orchestrator (apply) and fleet update — renders the
// same resolved addon set for a given cluster rather than each reimplementing
// the resolve-merge-template sequence and risking the two diverging.
func ResolveForCluster(ctx context.Context, resolver Resolver, spec core.ClusterSpec) (core.Profile, error) {
	profile, err := resolver.Resolve(ctx, spec.Profile)
	if err != nil {
		return core.Profile{}, fmt.Errorf("resolving profile %s for %s: %w", spec.Profile, spec.ID, err)
	}
	profile = profile.ForProvider(spec.Provider)
	profile = withArgoCDAddon(profile)

	merged, err := Merge(profile, spec.Overrides)
	if err != nil {
		return core.Profile{}, fmt.Errorf("applying overrides for %s: %w", spec.ID, err)
	}

	return argocd.ApplyProfileIngressDefaults(spec.Access, merged), nil
}

// withArgoCDAddon ensures profile always carries an "argocd" catalog entry,
// defaulting to argocd.DefaultAddon when the tier doesn't track one of its
// own (true below tier-standard). Argo CD is installed on every tier
// regardless of whether the catalog tracks it, so without this a
// cluster.yaml override naming "argocd" on a tier that doesn't carry it
// would fail Merge with ErrUnknownOverride even though the addon is always
// installed.
func withArgoCDAddon(profile core.Profile) core.Profile {
	if _, ok := profile.Addon(argocd.ReleaseName); ok {
		return profile
	}
	profile.Addons = append(append([]core.AddonRef(nil), profile.Addons...), argocd.DefaultAddon)
	return profile
}
