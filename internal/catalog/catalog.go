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

// tierSmall is the builtin stand-in for the real catalog entry M4 defines
// from the platform-profiles repo: the full addon set named in the
// implementation plan (CNI, cert-manager, Gateway API, ESO, Kyverno
// baseline, Cluster Autoscaler, kube-prometheus-stack, Fluent Bit, OpenCost,
// ExternalDNS, fleet-status-reporter), built from real public Helm charts
// so it renders and installs as-is rather than standing in for a shape
// that's still to be decided. Moving this into an actual platform-profiles
// repo (M4's still-open item) is a real infra action, not a code change —
// this only needs to be superseded, not rewritten, when that repo exists.
var tierSmall = core.Profile{
	Name:    "tier-small",
	Version: "1.0.0",
	Addons: []core.AddonRef{
		{
			// Cloud-default CNI is Cilium here; a cloud whose managed CNI
			// (e.g. EKS's default VPC CNI) is preferred over Cilium can
			// disable this addon via a per-cluster override (core.AddonOverride)
			// without any catalog change.
			Name:       "cilium",
			Chart:      "cilium",
			Repository: "https://helm.cilium.io",
			Version:    "1.16.3",
			Namespace:  "kube-system",
		},
		{
			Name:       "cert-manager",
			Chart:      "cert-manager",
			Repository: "https://charts.jetstack.io",
			Version:    "1.15.3",
			Namespace:  "cert-manager",
		},
		{
			// Gateway API's CRDs are cloud-agnostic, but the controller that
			// implements them is not (e.g. GKE Gateway controller vs. Cilium's
			// own Gateway API support). Like karpenter in tier-standard, this
			// carries no per-provider gate yet — core.AddonRef has no provider
			// constraint — so the real platform-profiles repo still needs to
			// pick the per-cloud implementation this addon stands in for.
			Name:       "gateway-api",
			Chart:      "gateway-api-crds",
			Repository: "https://charts.kubespin.dev",
			Version:    "0.1.0",
			Namespace:  "gateway-system",
		},
		{
			Name:       "external-secrets",
			Chart:      "external-secrets",
			Repository: "https://charts.external-secrets.io",
			Version:    "0.9.20",
			Namespace:  "external-secrets",
		},
		{
			Name:       "cluster-autoscaler",
			Chart:      "cluster-autoscaler",
			Repository: "https://kubernetes.github.io/autoscaler",
			Version:    "9.43.0",
			Namespace:  "kube-system",
		},
		{
			Name:       "kube-prometheus-stack",
			Chart:      "kube-prometheus-stack",
			Repository: "https://prometheus-community.github.io/helm-charts",
			Version:    "62.7.0",
			Namespace:  "monitoring",
		},
		{
			Name:       "fluent-bit",
			Chart:      "fluent-bit",
			Repository: "https://fluent.github.io/helm-charts",
			Version:    "0.47.10",
			Namespace:  "logging",
		},
		{
			Name:       "opencost",
			Chart:      "opencost",
			Repository: "https://opencost.github.io/opencost-helm-chart",
			Version:    "1.44.0",
			Namespace:  "opencost",
		},
		{
			Name:       "external-dns",
			Chart:      "external-dns",
			Repository: "https://kubernetes-sigs.github.io/external-dns",
			Version:    "1.15.0",
			Namespace:  "external-dns",
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
