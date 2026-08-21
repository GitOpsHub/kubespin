// Package catalog resolves a cluster's size (small/medium/large) into the
// addon set its addons.yaml should carry.
//
// Every size is defined here in code, in the builtin catalog — there is no
// external profiles repository to consult, and no version to pin. Changing
// what a size includes means shipping a new kubespin build. Resolver is
// still the seam internal/repo, the orchestrator, and the CLI go through, so
// a future backing store change would not ripple upstream, but today
// BuiltinResolver is the only implementation.
package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/GitOpsHub/kubespin/internal/argocd"
	"github.com/GitOpsHub/kubespin/internal/core"
)

// ErrProfileNotFound means the catalog has no profile matching the size.
var ErrProfileNotFound = errors.New("profile not found")

// Resolver resolves a cluster size to its full addon set.
type Resolver interface {
	Resolve(ctx context.Context, size core.ClusterSize) (core.Profile, error)
}

// BuiltinResolver serves a fixed, in-memory set of size profiles.
type BuiltinResolver struct {
	profiles map[core.ClusterSize]core.Profile
}

// NewBuiltinResolver builds a resolver over the builtin size catalog.
func NewBuiltinResolver() *BuiltinResolver {
	return &BuiltinResolver{profiles: map[core.ClusterSize]core.Profile{
		core.SizeSmall:  sizeSmall,
		core.SizeMedium: sizeMedium,
		core.SizeLarge:  sizeLarge,
	}}
}

// Resolve returns the profile for size.
func (r *BuiltinResolver) Resolve(_ context.Context, size core.ClusterSize) (core.Profile, error) {
	profile, ok := r.profiles[size]
	if !ok {
		return core.Profile{}, fmt.Errorf("%w: %s", ErrProfileNotFound, size)
	}
	return profile, nil
}

// baseAddons is the addon set every size carries, regardless of cloud:
// CNI, cert-manager, Gateway API, ESO, Kyverno baseline, an autoscaler,
// kube-prometheus-stack, Fluent Bit, OpenCost, ExternalDNS, Argo CD, and
// fleet-status-reporter.
//
// Argo CD and the autoscaler are unconditional here rather than added by a
// higher tier: every cluster gets Argo CD (catalog.ResolveForCluster also
// defends this via withArgoCDAddon, in case a future size ever omits it),
// and every cluster gets a node autoscaler appropriate to its cloud —
// Karpenter on AWS (EKS-only technology, no GCP/Azure port exists) and
// cluster-autoscaler on GCP/Azure. Profile.ForProvider is what makes the two
// mutually exclusive per cluster: each carries a Providers gate naming the
// clouds it applies to, so a given cluster only ever renders one of them.
var baseAddons = []core.AddonRef{
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
		// own Gateway API support). This carries no per-provider gate yet —
		// core.AddonRef has no provider constraint — so picking the
		// per-cloud implementation this addon stands in for is still open.
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
		// GCP/Azure only: Karpenter (below) covers AWS. Without this gate the
		// two autoscalers would fight over the same nodes on an AWS cluster.
		Name:       "cluster-autoscaler",
		Chart:      "cluster-autoscaler",
		Repository: "https://kubernetes.github.io/autoscaler",
		Version:    "9.43.0",
		Namespace:  "kube-system",
		Providers:  []core.Provider{core.ProviderGCP, core.ProviderAzure},
	},
	{
		// AWS only: Karpenter is EKS-specific technology, with no GCP/Azure
		// equivalent — cluster-autoscaler (above) is what those clouds get
		// instead. core.Profile.ForProvider drops whichever one does not
		// apply before an override patch or Argo CD ever sees it.
		Name:       "karpenter",
		Chart:      "karpenter",
		Repository: "oci://public.ecr.aws/karpenter/karpenter",
		Version:    "1.0.6",
		Namespace:  "karpenter",
		Providers:  []core.Provider{core.ProviderAWS},
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
	argocd.DefaultAddon,
}

// sizeSmall is the base addon set alone: Argo CD and a cloud-appropriate
// autoscaler, nothing else layered on top.
var sizeSmall = core.Profile{
	Name:   "small",
	Addons: withAddons(baseAddons),
}
