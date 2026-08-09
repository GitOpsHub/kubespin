package core

import (
	"errors"
	"fmt"
	"regexp"
)

// namePattern constrains profile and addon names to a DNS-label-ish shape, since
// both surface as Argo CD Application names.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,61}[a-z0-9]$`)

// ProfileRef points at a versioned profile in the platform-profiles repository.
// Pinning the version is what makes `fleet update` a deliberate, staged action
// rather than an implicit consequence of someone merging to the catalog.
type ProfileRef struct {
	Name    string `yaml:"name" json:"name"`
	Version string `yaml:"version" json:"version"`
}

// Validate reports whether the reference is well formed.
func (r ProfileRef) Validate() error {
	var errs []error
	if !namePattern.MatchString(r.Name) {
		errs = append(errs, fmt.Errorf("%w: profile name %q is not a valid name", ErrInvalidSpec, r.Name))
	}
	if r.Version == "" {
		errs = append(errs, fmt.Errorf("%w: profile %q: version is required", ErrInvalidSpec, r.Name))
	}
	return errors.Join(errs...)
}

func (r ProfileRef) String() string { return r.Name + "@" + r.Version }

// AddonRef is one Helm chart delivered to a cluster. Each addon becomes its own
// Argo CD Application, so addons sync and fail independently of one another.
type AddonRef struct {
	Name       string         `yaml:"name" json:"name"`
	Chart      string         `yaml:"chart" json:"chart"`
	Repository string         `yaml:"repository" json:"repository"`
	Version    string         `yaml:"version" json:"version"`
	Namespace  string         `yaml:"namespace" json:"namespace"`
	Values     map[string]any `yaml:"values,omitempty" json:"values,omitempty"`
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
	return errors.Join(errs...)
}

// Profile is a resolved tier from the platform-profiles catalog: the addon set
// a cluster gets before any per-cluster override patch is applied.
type Profile struct {
	Name    string     `yaml:"name" json:"name"`
	Version string     `yaml:"version" json:"version"`
	Addons  []AddonRef `yaml:"addons" json:"addons"`
}

// Ref returns the reference identifying this profile.
func (p Profile) Ref() ProfileRef { return ProfileRef{Name: p.Name, Version: p.Version} }

// Validate checks the profile and every addon it carries, rejecting duplicate
// addon names — two Argo CD Applications cannot share a name.
func (p Profile) Validate() error {
	var errs []error
	if err := p.Ref().Validate(); err != nil {
		errs = append(errs, err)
	}
	if len(p.Addons) == 0 {
		errs = append(errs, fmt.Errorf("%w: profile %q: at least one addon is required", ErrInvalidSpec, p.Name))
	}
	seen := make(map[string]struct{}, len(p.Addons))
	for _, a := range p.Addons {
		if err := a.Validate(); err != nil {
			errs = append(errs, err)
		}
		if _, dup := seen[a.Name]; dup && a.Name != "" {
			errs = append(errs, fmt.Errorf("%w: profile %q: duplicate addon name %q", ErrInvalidSpec, p.Name, a.Name))
		}
		seen[a.Name] = struct{}{}
	}
	return errors.Join(errs...)
}
