package catalog

import (
	"errors"
	"fmt"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// ErrUnknownOverride means an override patch names an addon the profile does
// not carry.
var ErrUnknownOverride = errors.New("override does not match any addon in the profile")

// Merge applies a cluster's override patch onto a resolved profile.
//
// It patches addons in place — an override changes a field, it never adds a
// new addon or duplicates one — which is what keeps the resolved addons.yaml
// from diverging from the catalog it was derived from. An override naming an
// addon the profile does not carry is an error: a typo in a per-cluster patch
// should surface at apply time, not be silently ignored.
func Merge(profile core.Profile, overrides []core.AddonOverride) (core.Profile, error) {
	if len(overrides) == 0 {
		return profile, nil
	}

	byName := make(map[string]int, len(profile.Addons))
	for i, a := range profile.Addons {
		byName[a.Name] = i
	}

	merged := profile
	merged.Addons = append([]core.AddonRef(nil), profile.Addons...)

	disabled := make(map[string]bool, len(overrides))
	for _, o := range overrides {
		idx, ok := byName[o.Name]
		if !ok {
			return core.Profile{}, fmt.Errorf("%w: %q in profile %s", ErrUnknownOverride, o.Name, profile.Ref())
		}

		addon := merged.Addons[idx]
		if o.Version != "" {
			addon.Version = o.Version
		}
		if o.Values != nil {
			addon.Values = MergeValues(addon.Values, o.Values)
		}
		merged.Addons[idx] = addon

		if o.Disable {
			disabled[o.Name] = true
		}
	}

	if len(disabled) > 0 {
		kept := merged.Addons[:0]
		for _, a := range merged.Addons {
			if !disabled[a.Name] {
				kept = append(kept, a)
			}
		}
		merged.Addons = kept
	}

	return merged, nil
}

// MergeValues overlays override values onto the profile's, one level deep.
// One level is enough for the shape addon values take in practice — flat
// Helm value keys, occasionally one nested map — and going deeper would mean
// guessing at merge semantics (replace vs. deep-merge a slice, for instance)
// that only the addon's own chart can really judge.
//
// Exported so callers outside this package (orchestrator's argoCDAddon) can
// apply the same one-level overlay to argocd.DefaultAddon, which never
// appears in a profile's own Addons list for Merge to patch in place.
func MergeValues(base, override map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(override))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range override {
		merged[k] = v
	}
	return merged
}
