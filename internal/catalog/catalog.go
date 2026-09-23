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
// kube-prometheus-stack, Fluent Bit, OpenCost, ExternalDNS, and Argo CD.
//
// Argo CD and the autoscaler are unconditional here rather than added by a
// higher tier: every cluster gets Argo CD (catalog.ResolveForCluster also
// defends this via withArgoCDAddon, in case a future size ever omits it),
// and every cluster gets cluster-autoscaler. It appears twice — once
// configured for AWS, once for GCP/Azure — and Profile.ForProvider keeps
// exactly one per cluster, since each carries a Providers gate naming the
// clouds it applies to.
var baseAddons = []core.AddonRef{
	{
		// No CNI addon: every cloud's managed CNI (EKS's VPC CNI, GKE's,
		// AKS's) is already running when Argo CD first syncs. Layering a
		// second CNI on top (chart-default Cilium did, with an overlay pod
		// CIDR of 10.0.0.0/8 over the VPC's own range) hijacks node routing
		// and takes every node NotReady.
		// GCP/Azure only: on AWS the EKS add-on of the same name replaces
		// this chart (internal/provisioner/aws/addons.go), installed and
		// upgraded by EKS rather than Argo CD.
		Name:       "cert-manager",
		Chart:      "cert-manager",
		Repository: "https://charts.jetstack.io",
		Version:    "1.15.3",
		Namespace:  "cert-manager",
		Providers:  []core.Provider{core.ProviderGCP, core.ProviderAzure},
	},
	{
		// Gateway API's CRDs are cloud-agnostic, but the controller that
		// implements them is not (e.g. GKE Gateway controller vs. an
		// ingress controller's own Gateway API support). This carries no
		// per-provider gate yet —
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
		// GCP/Azure: the same chart as the AWS entry below. The two share a
		// name, and their Providers gates never overlap, so
		// core.Profile.ForProvider leaves exactly one per cluster and an
		// override naming "cluster-autoscaler" works on every cloud.
		Name:       "cluster-autoscaler",
		Chart:      "cluster-autoscaler",
		Repository: "https://kubernetes.github.io/autoscaler",
		Version:    "9.43.0",
		Namespace:  core.ClusterAutoscalerNamespace,
		Providers:  []core.Provider{core.ProviderGCP, core.ProviderAzure},
	},
	{
		// AWS: resizes the EKS managed node groups kubespin creates, found by
		// the k8s.io/cluster-autoscaler/<cluster> tag EKS puts on every
		// managed node group's Auto Scaling group. Its AWS permissions come
		// from a role the AWS provisioner binds to this service account
		// through EKS Pod Identity, so the values carry no role ARN.
		//
		// The extraArgs lean toward the lowest cost: least-waste packs pods
		// onto the fewest nodes, and scale-down may remove a node running
		// kube-system or emptyDir pods after 5 idle minutes rather than
		// keeping it forever.
		Name:       "cluster-autoscaler",
		Chart:      "cluster-autoscaler",
		Repository: "https://kubernetes.github.io/autoscaler",
		Version:    "9.43.0",
		Namespace:  core.ClusterAutoscalerNamespace,
		Providers:  []core.Provider{core.ProviderAWS},
		Values: map[string]any{
			"cloudProvider": "aws",
			"awsRegion":     RegionPlaceholder,
			"autoDiscovery": map[string]any{"clusterName": ClusterIDPlaceholder},
			"rbac": map[string]any{"serviceAccount": map[string]any{
				"create": true,
				"name":   core.ClusterAutoscalerServiceAccount,
			}},
			"extraArgs": map[string]any{
				"expander":                      "least-waste",
				"balance-similar-node-groups":   true,
				"skip-nodes-with-system-pods":   false,
				"skip-nodes-with-local-storage": false,
				"scale-down-unneeded-time":      "5m",
				"scale-down-delay-after-add":    "5m",
			},
		},
	},
	{
		Name:       "kube-prometheus-stack",
		Chart:      "kube-prometheus-stack",
		Repository: "https://prometheus-community.github.io/helm-charts",
		Version:    "62.7.0",
		Namespace:  "monitoring",
	},
	{
		// GCP/Azure only: on AWS the EKS add-on of the same name replaces
		// this chart (internal/provisioner/aws/addons.go), installed and
		// upgraded by EKS rather than Argo CD.
		Name:       "fluent-bit",
		Chart:      "fluent-bit",
		Repository: "https://fluent.github.io/helm-charts",
		Version:    "0.47.10",
		Namespace:  "logging",
		Providers:  []core.Provider{core.ProviderGCP, core.ProviderAzure},
	},
	{
		Name:       "opencost",
		Chart:      "opencost",
		Repository: "https://opencost.github.io/opencost-helm-chart",
		Version:    "1.44.0",
		Namespace:  "opencost",
	},
	{
		// GCP/Azure only: on AWS the EKS add-on of the same name replaces
		// this chart (internal/provisioner/aws/addons.go), installed and
		// upgraded by EKS rather than Argo CD.
		Name:       "external-dns",
		Chart:      "external-dns",
		Repository: "https://kubernetes-sigs.github.io/external-dns",
		Version:    "1.15.0",
		Namespace:  "external-dns",
		Providers:  []core.Provider{core.ProviderGCP, core.ProviderAzure},
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
	argocd.DefaultAddon,
}

// sizeSmall is the base addon set alone: Argo CD and a cloud-appropriate
// autoscaler, nothing else layered on top.
var sizeSmall = core.Profile{
	Name:   "small",
	Addons: withAddons(baseAddons),
}
