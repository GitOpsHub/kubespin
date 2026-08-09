// Package catalog resolves a ProfileRef into the addon set a cluster's
// addons.yaml should carry.
//
// This is a placeholder implementation: a single builtin tier-small profile,
// enough for the repo-seeding and idempotent-diff machinery in internal/repo
// to have something real to render. Milestone 4 replaces the backing store
// with the platform-profiles repository and adds per-cluster override
// patches; Resolver is the seam that change happens behind, so nothing
// upstream of it (internal/repo, the orchestrator, the CLI) has to change
// when it does.
package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// ErrProfileNotFound means the catalog has no profile matching the reference.
var ErrProfileNotFound = errors.New("profile not found")

// Resolver resolves a profile reference to its full addon set.
type Resolver interface {
	Resolve(ctx context.Context, ref core.ProfileRef) (core.Profile, error)
}

// BuiltinResolver serves a fixed, in-memory set of profiles.
type BuiltinResolver struct {
	profiles map[string]core.Profile
}

// NewBuiltinResolver builds a resolver over the builtin profile set.
func NewBuiltinResolver() *BuiltinResolver {
	return &BuiltinResolver{profiles: map[string]core.Profile{
		tierSmall.Ref().String():     tierSmall,
		tierStandard.Ref().String():  tierStandard,
		tierRegulated.Ref().String(): tierRegulated,
	}}
}

// Resolve returns the profile matching ref, including its version: two
// versions of the same profile name are different entries, the way `fleet
// update` needs them to be.
func (r *BuiltinResolver) Resolve(_ context.Context, ref core.ProfileRef) (core.Profile, error) {
	profile, ok := r.profiles[ref.String()]
	if !ok {
		return core.Profile{}, fmt.Errorf("%w: %s", ErrProfileNotFound, ref)
	}
	return profile, nil
}

// tierSmall is a placeholder for the real catalog entry M4 defines from the
// platform-profiles repo: this set stands in for the full one (CNI, Gateway
// API, ESO, Cluster Autoscaler, kube-prometheus-stack, Fluent Bit, OpenCost)
// documented in the implementation plan. It carries enough real addons —
// including an ingress controller and Kyverno's baseline policy — for the
// access-mode-aware templating and public-exposure-deny rule M5 adds to have
// something real to apply to.
var tierSmall = core.Profile{
	Name:    "tier-small",
	Version: "1.0.0",
	Addons: []core.AddonRef{
		{
			Name:       "cert-manager",
			Chart:      "cert-manager",
			Repository: "https://charts.jetstack.io",
			Version:    "1.15.3",
			Namespace:  "cert-manager",
		},
		{
			Name:       "ingress-nginx",
			Chart:      "ingress-nginx",
			Repository: "https://kubernetes.github.io/ingress-nginx",
			Version:    "4.11.2",
			Namespace:  "ingress-nginx",
			// exposure defaults to "internal" until access-mode templating
			// (internal/argocd.ApplyIngressDefaults) overlays the resolved
			// value; a public cluster's ingress addon can request "external"
			// here to opt in.
			Values: map[string]any{"ingress": map[string]any{"exposure": "internal"}},
		},
		{
			Name:       "kyverno",
			Chart:      "kyverno",
			Repository: "https://kyverno.github.io/kyverno",
			Version:    "3.2.6",
			Namespace:  "kyverno",
		},
		{
			Name:       "kyverno-policies",
			Chart:      "kyverno-policies-baseline",
			Repository: "https://charts.kubespin.dev",
			Version:    "0.1.0",
			Namespace:  "kyverno",
			// publicExposureDeny enforces the admission-time rule the project's
			// CLAUDE.md requires regardless of access mode: a Service or
			// Ingress that would expose the cluster publicly is rejected
			// unless Access is public and the addon requesting it opted in via
			// ingress.exposure, matching the same default this profile's own
			// ingress-nginx addon carries.
			Values: map[string]any{"policies": map[string]any{"publicExposureDeny": true}},
		},
		{
			Name:       "fleet-status-reporter",
			Chart:      "fleet-status-reporter",
			Repository: "https://charts.kubespin.dev",
			Version:    "0.1.0",
			Namespace:  "kubespin-system",
		},
	},
}
