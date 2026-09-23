package argocd

import (
	"github.com/GitOpsHub/kubespin/internal/core"
)

// Exposure is how an ingress or Gateway API addon's load balancer is
// reachable.
type Exposure string

// Exposure modes. Internal is the default in every case but one: see
// ResolveExposure.
const (
	ExposureInternal Exposure = "internal"
	ExposureExternal Exposure = "external"
)

// ResolveExposure applies the public/private-aware ingress default from the
// project's CLAUDE.md: internal load balancer unless the cluster is
// core.AccessPublic *and* the addon itself asks to be external. A private
// cluster overrides any addon-level request — there is no public endpoint
// for an externally exposed load balancer to sit in front of, so honouring
// "external" there would be silently wrong rather than merely surprising.
func ResolveExposure(access core.Access, requested Exposure) Exposure {
	if access == core.AccessPublic && requested == ExposureExternal {
		return ExposureExternal
	}
	return ExposureInternal
}

// requestedExposure reads an addon's own ingress.exposure value, defaulting
// to internal when the addon does not set one.
func requestedExposure(values map[string]any) Exposure {
	ingress, ok := values["ingress"].(map[string]any)
	if !ok {
		return ExposureInternal
	}
	exposure, ok := ingress["exposure"].(string)
	if !ok {
		return ExposureInternal
	}
	return Exposure(exposure)
}

// internalLoadBalancerAnnotations ask each cloud for an internal load
// balancer. Every cloud ignores the others' keys, so they are applied
// together rather than per provider, which keeps cloud conditionals out of
// rendering. AWS gets both forms: the first for its cloud controller, the
// second for the AWS Load Balancer Controller when one is installed.
var internalLoadBalancerAnnotations = map[string]any{
	"service.beta.kubernetes.io/aws-load-balancer-internal":   "true",
	"service.beta.kubernetes.io/aws-load-balancer-scheme":     "internal",
	"networking.gke.io/load-balancer-type":                    "Internal",
	"service.beta.kubernetes.io/azure-load-balancer-internal": "true",
}

// ApplyIngressDefaults overlays the resolved exposure onto an ingress/Gateway
// addon's values.
//
// It always writes an explicit ingress.exposure, and an internal-lb flag chart
// authors can key an annotation off, rather than leaving the decision
// implicit in whatever the profile or override happened to say. That is what
// makes the rendered Application prove the access-mode default was applied,
// instead of trusting that whoever wrote the profile got it right.
//
// An addon whose values declare a top-level service of type LoadBalancer
// also gets that Service made to match: internal exposure adds every cloud's
// internal-load-balancer annotation, and external exposure limits the load
// balancer to the cluster's authorized CIDRs (WithAuthorizedCIDRs) through
// loadBalancerSourceRanges, when there are any.
//
// addon's own Values map is not mutated: the returned AddonRef carries a new
// one, so a caller iterating a profile's addons cannot accidentally leak this
// addon's ingress defaults into another.
func ApplyIngressDefaults(access core.Access, addon core.AddonRef, opts ...Option) core.AddonRef {
	o := resolve(opts)
	exposure := ResolveExposure(access, requestedExposure(addon.Values))

	values := make(map[string]any, len(addon.Values)+1)
	for k, v := range addon.Values {
		values[k] = v
	}

	ingress := map[string]any{}
	if existing, ok := addon.Values["ingress"].(map[string]any); ok {
		for k, v := range existing {
			ingress[k] = v
		}
	}
	ingress["exposure"] = string(exposure)
	ingress["internal"] = exposure == ExposureInternal
	values["ingress"] = ingress

	if service, ok := addon.Values["service"].(map[string]any); ok && service["type"] == "LoadBalancer" {
		values["service"] = loadBalancerService(service, exposure, o.authorizedCIDRs)
	}

	addon.Values = values
	return addon
}

// loadBalancerService returns a copy of service shaped for exposure.
func loadBalancerService(service map[string]any, exposure Exposure, authorizedCIDRs []string) map[string]any {
	out := make(map[string]any, len(service)+2)
	for k, v := range service {
		out[k] = v
	}

	if exposure == ExposureInternal {
		annotations := map[string]any{}
		if existing, ok := service["annotations"].(map[string]any); ok {
			for k, v := range existing {
				annotations[k] = v
			}
		}
		for k, v := range internalLoadBalancerAnnotations {
			annotations[k] = v
		}
		out["annotations"] = annotations
		return out
	}

	if len(authorizedCIDRs) > 0 {
		ranges := make([]any, len(authorizedCIDRs))
		for i, cidr := range authorizedCIDRs {
			ranges[i] = cidr
		}
		out["loadBalancerSourceRanges"] = ranges
	}
	return out
}

// ApplyProfileIngressDefaults applies ApplyIngressDefaults to every addon in
// profile that already declares an ingress.exposure value, leaving every
// other addon untouched.
//
// Scoping it that way — rather than patching every addon uniformly — matters:
// an addon with no opinion about ingress should not gain an empty
// ingress:{...} block in its rendered Application just because some other
// addon in the profile happens to be a load balancer.
func ApplyProfileIngressDefaults(access core.Access, profile core.Profile, opts ...Option) core.Profile {
	o := resolve(opts)

	patched := profile
	patched.Addons = make([]core.AddonRef, len(profile.Addons))
	for i, addon := range profile.Addons {
		if _, ok := addon.Values["ingress"]; ok {
			requested := requestedExposure(addon.Values)
			addon = ApplyIngressDefaults(access, addon, opts...)
			if requested == ExposureExternal && ResolveExposure(access, requested) == ExposureInternal {
				// Worth saying out loud: the operator asked for an external
				// load balancer and is not getting one.
				o.logger.Warn("Forced Addon Exposure Internal",
					"addon", addon.Name, "access", access, "reason", "cluster access mode does not allow external exposure")
			}
		}
		patched.Addons[i] = addon
	}
	return patched
}
