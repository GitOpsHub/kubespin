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
// kube-prometheus-stack, Fluent Bit, ExternalDNS, and Argo CD.
//
// Argo CD and the autoscaler are unconditional here rather than added by a
// higher tier: every cluster gets Argo CD (catalog.ResolveForCluster also
// defends this via withArgoCDAddon, in case a future size ever omits it),
// and every EKS cluster gets cluster-autoscaler. GKE and AKS get no chart
// for it: their node pools autoscale natively.
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
		// The standard-channel Gateway API CRDs, from Envoy Gateway's CRD
		// chart with only the Gateway API half enabled (it is the one
		// published chart that ships them alone). The CRDs are
		// cloud-agnostic; which controller implements them is still open.
		Name:       "gateway-api",
		Chart:      "gateway-crds-helm",
		Repository: "oci://docker.io/envoyproxy/gateway-crds-helm",
		Version:    "1.9.1",
		Namespace:  "gateway-system",
		Values: map[string]any{"crds": map[string]any{
			"gatewayAPI":   map[string]any{"enabled": true, "channel": "standard"},
			"envoyGateway": map[string]any{"enabled": false},
		}},
	},
	{
		Name:       "external-secrets",
		Chart:      "external-secrets",
		Repository: "https://charts.external-secrets.io",
		Version:    "0.9.20",
		Namespace:  "external-secrets",
	},
	{
		// 9.59.x runs cluster-autoscaler 1.35, the release closest to the
		// Kubernetes versions kubespin creates; the autoscaler is meant to
		// track the cluster's minor version.
		//
		// AWS only: GKE and AKS node pools autoscale natively (kubespin
		// enables it on every pool it creates), and this chart has no
		// credentials to drive either cloud's instance groups anyway.
		//
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
		Version:    "9.59.0",
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
		// Prometheus and Grafana are each served from an internet-facing
		// LoadBalancer on every cluster, private included, like argocd-server:
		// operator UIs depend on reaching them. Access-mode templating
		// (internal/argocd.ApplyIngressDefaults) only reshapes a top-level
		// service, so it leaves both as declared here.
		Name:       "kube-prometheus-stack",
		Chart:      "kube-prometheus-stack",
		Repository: "https://prometheus-community.github.io/helm-charts",
		Version:    "62.7.0",
		Namespace:  "monitoring",
		Values: map[string]any{
			"prometheus": map[string]any{"service": map[string]any{"type": "LoadBalancer"}},
			"grafana":    map[string]any{"service": map[string]any{"type": "LoadBalancer"}},
		},
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
		// 3.9.x: charts before 3.3 ran their report-cleanup CronJobs on
		// bitnami/kubectl tags that Bitnami has since pulled from Docker Hub,
		// so those jobs sat in ImagePullBackOff.
		Name:       "kyverno",
		Chart:      "kyverno",
		Repository: "https://kyverno.github.io/kyverno",
		Version:    "3.9.1",
		Namespace:  "kyverno",
	},
	{
		// Kyverno's own Pod Security Standards policies, at the baseline
		// level, in Audit mode: violations are reported (PolicyReports), not
		// blocked, so no addon in any size can be refused admission by
		// them. sizeLarge swaps in the restricted level.
		Name:       "kyverno-policies",
		Chart:      "kyverno-policies",
		Repository: "https://kyverno.github.io/kyverno",
		Version:    "3.9.1",
		Namespace:  "kyverno",
		Values: map[string]any{
			"podSecurityStandard":     "baseline",
			"validationFailureAction": "Audit",
		},
	},
	argocd.DefaultAddon,
}

// sizeSmall is the base addon set alone: Argo CD and a cloud-appropriate
// autoscaler, nothing else layered on top.
var sizeSmall = core.Profile{
	Name:   "small",
	Addons: withAddons(baseAddons),
}
