package core

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
)

// namePattern constrains profile and addon names to a DNS-label-ish shape, since
// both surface as Argo CD Application names.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,61}[a-z0-9]$`)

// The namespace and service account the catalog's cluster-autoscaler addon
// runs as. Shared by the catalog, which names it in the chart's values, and
// any provisioner that binds cloud permissions to it (AWS's Pod Identity
// association), so the two cannot drift apart.
const (
	ClusterAutoscalerNamespace      = "kube-system"
	ClusterAutoscalerServiceAccount = "cluster-autoscaler"
)

// AddonRef is one Helm chart delivered to a cluster. Each addon becomes its own
// Argo CD Application, so addons sync and fail independently of one another.
type AddonRef struct {
	Name       string         `yaml:"name" json:"name"`
	Chart      string         `yaml:"chart" json:"chart"`
	Repository string         `yaml:"repository" json:"repository"`
	Version    string         `yaml:"version" json:"version"`
	Namespace  string         `yaml:"namespace" json:"namespace"`
	Values     map[string]any `yaml:"values,omitempty" json:"values,omitempty"`

	// Providers restricts this addon to the named clouds, e.g. the
	// AWS-configured cluster-autoscaler entry. Empty means every provider —
	// most addons in the catalog are cloud-agnostic and leave this unset. Profile.ForProvider is what acts on
	// it; a profile resolved without going through ForProvider still carries
	// every addon regardless of this field.
	Providers []Provider `yaml:"providers,omitempty" json:"providers,omitempty"`
}

// Validate reports whether the addon reference is complete enough to render.
func (a AddonRef) Validate() error {
	var errs []error
	if !namePattern.MatchString(a.Name) {
		errs = append(errs, fmt.Errorf("%w: addon name %q is not a valid name", ErrInvalidSpec, a.Name))
	}
	if a.Chart == "" {
		errs = append(errs, fmt.Errorf("%w: addon %q: chart is required", ErrInvalidSpec, a.Name))
	}
	if a.Repository == "" {
		errs = append(errs, fmt.Errorf("%w: addon %q: repository is required", ErrInvalidSpec, a.Name))
	}
	// Version is mandatory: an unpinned addon makes a cluster's resolved state
	// unreproducible, which breaks the .state.yaml no-op guarantee.
	if a.Version == "" {
		errs = append(errs, fmt.Errorf("%w: addon %q: version is required", ErrInvalidSpec, a.Name))
	}
	if a.Namespace == "" {
		errs = append(errs, fmt.Errorf("%w: addon %q: namespace is required", ErrInvalidSpec, a.Name))
	}
	for _, p := range a.Providers {
		if !p.Valid() {
			errs = append(errs, fmt.Errorf("%w: addon %q: provider %q is not supported", ErrInvalidSpec, a.Name, p))
		}
	}
	return errors.Join(errs...)
}

// SupportsProvider reports whether the addon should be delivered to a cluster
// on provider: true when Providers is empty (every provider) or provider is
// named in it.
func (a AddonRef) SupportsProvider(provider Provider) bool {
	return len(a.Providers) == 0 || slices.Contains(a.Providers, provider)
}

// AddonOverride patches one addon of a resolved profile, by name, as part of a
// cluster's per-cluster override patch.
//
// Every field but Name is optional and additive: a zero Version leaves the
// profile's pinned version alone, and a nil Values leaves the profile's
// values alone. That is what lets an override patch say only what it changes
// rather than restate the whole addon, which is also why it never introduces
// a new addon — Name must match one the profile already carries.
type AddonOverride struct {
	Name    string         `yaml:"name" json:"name"`
	Version string         `yaml:"version,omitempty" json:"version,omitempty"`
	Values  map[string]any `yaml:"values,omitempty" json:"values,omitempty"`

	// Disable drops the addon from the resolved set entirely, e.g. a cluster
	// that provides its own ExternalDNS and does not want the profile's.
	Disable bool `yaml:"disable,omitempty" json:"disable,omitempty"`
}

// Validate reports whether the override is well formed. It cannot check that
// Name matches an addon in the profile being overridden — that is a property
// of a (profile, override) pair, not of the override alone — so callers doing
// that check (internal/catalog.Merge) report the more specific error.
func (o AddonOverride) Validate() error {
	if !namePattern.MatchString(o.Name) {
		return fmt.Errorf("%w: override name %q is not a valid name", ErrInvalidSpec, o.Name)
	}
	return nil
}

// Profile is a resolved size tier from the builtin catalog: the addon set a
// cluster gets before any per-cluster override patch is applied.
type Profile struct {
	Name   string     `yaml:"name" json:"name"`
	Addons []AddonRef `yaml:"addons" json:"addons"`
}

// ForProvider returns a copy of p with every addon that does not support
// provider dropped (see AddonRef.SupportsProvider). Callers resolve a profile
// for a specific cluster's provider through this before applying override
// patches, so an addon configured for one cloud never renders into another
// cloud's addons.yaml, and an override naming it on those clouds
// correctly fails as unknown rather than silently applying.
func (p Profile) ForProvider(provider Provider) Profile {
	out := p
	out.Addons = make([]AddonRef, 0, len(p.Addons))
	for _, a := range p.Addons {
		if a.SupportsProvider(provider) {
			out.Addons = append(out.Addons, a)
		}
	}
	return out
}

// ForAutopilot returns a copy of p with every addon dropped when autopilot is
// true. GKE Autopilot and EKS Auto Mode manage compute, storage, load
// balancing, and node autoscaling themselves — none of a size tier's addons
// (cluster-autoscaler, CSI drivers, ingress controllers, and so on) are
// meaningful on top of that, so an Autopilot/Auto Mode cluster gets nothing
// from the catalog beyond Argo CD itself (added back by
// catalog.withArgoCDAddon regardless of this).
func (p Profile) ForAutopilot(autopilot bool) Profile {
	if !autopilot {
		return p
	}
	out := p
	out.Addons = nil
	return out
}

// Addon returns the addon named name from p's addon set, if present.
func (p Profile) Addon(name string) (AddonRef, bool) {
	for _, a := range p.Addons {
		if a.Name == name {
			return a, true
		}
	}
	return AddonRef{}, false
}

// Validate checks the profile and every addon it carries, rejecting duplicate
// addon names — two Argo CD Applications cannot share a name. Two entries
// may share a name only when their Providers gates are disjoint (one
// configured per cloud): ForProvider then keeps at most one of them for any
// cluster, so no cluster ever renders both.
func (p Profile) Validate() error {
	var errs []error
	if p.Name == "" {
		errs = append(errs, fmt.Errorf("%w: profile name is required", ErrInvalidSpec))
	}
	if len(p.Addons) == 0 {
		errs = append(errs, fmt.Errorf("%w: profile %q: at least one addon is required", ErrInvalidSpec, p.Name))
	}
	seen := make(map[string][]AddonRef, len(p.Addons))
	for _, a := range p.Addons {
		if err := a.Validate(); err != nil {
			errs = append(errs, err)
		}
		for _, prev := range seen[a.Name] {
			if a.Name != "" && prev.sharesProviderWith(a) {
				errs = append(errs, fmt.Errorf("%w: profile %q: duplicate addon name %q", ErrInvalidSpec, p.Name, a.Name))
				break
			}
		}
		seen[a.Name] = append(seen[a.Name], a)
	}
	return errors.Join(errs...)
}

// sharesProviderWith reports whether some provider would keep both a and b
// through ForProvider. An empty Providers list means every provider.
func (a AddonRef) sharesProviderWith(b AddonRef) bool {
	if len(a.Providers) == 0 || len(b.Providers) == 0 {
		return true
	}
	for _, p := range a.Providers {
		if slices.Contains(b.Providers, p) {
			return true
		}
	}
	return false
}
