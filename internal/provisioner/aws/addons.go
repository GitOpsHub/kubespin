package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

// EKS add-on names. Wherever AWS ships an EKS add-on for a component a
// cluster needs, kubespin installs that instead of a Helm chart: EKS owns
// its lifecycle, version compatibility with the control plane, and
// in-place upgrades, and it binds AWS permissions itself through Pod
// Identity. The catalog gates the Helm equivalents of these off AWS
// (core.AddonRef.Providers), so the two never both run on one cluster.
const (
	addonVPCCNI           = "vpc-cni"
	addonKubeProxy        = "kube-proxy"
	addonCoreDNS          = "coredns"
	addonPodIdentityAgent = "eks-pod-identity-agent"
	addonEBSCSIDriver     = "aws-ebs-csi-driver"
	addonEFSCSIDriver     = "aws-efs-csi-driver"
	addonCertManager      = "cert-manager"
	addonExternalDNS      = "external-dns"
	addonFluentBit        = "fluent-bit"
)

// managedAddon is one EKS add-on and how kubespin configures it.
type managedAddon struct {
	name string

	// config is the add-on's configurationValues. Nil leaves EKS's defaults.
	config map[string]any

	// identity, when set, is the AWS role the add-on's service account gets
	// through EKS Pod Identity. Nil means the add-on needs no AWS permissions.
	identity *addonIdentity

	// autoMode keeps the add-on on an EKS Auto Mode cluster. Auto Mode runs
	// its own networking, DNS, block storage and Pod Identity agent, so only
	// what it lacks (EFS) is installed there.
	autoMode bool
}

// addonIdentity is the role an add-on's service account assumes.
type addonIdentity struct {
	serviceAccount  string
	role            string
	managedPolicies []string
	inlinePolicy    map[string]any
}

// vpcCNIConfig turns on prefix delegation: each ENI slot hands out a /28
// prefix (16 addresses) instead of one address. Without it a node's pod
// limit is set by its ENI count — 17 on a t3.medium, which the default
// addon set alone outgrows, stranding pods as "Too many pods" on a node
// with CPU and memory to spare. With it EKS raises the limit to 110 (its
// recommended ceiling below 30 vCPUs), so small nodes fill up on resources
// rather than on addresses, and the cluster needs fewer of them.
//
// WARM_PREFIX_TARGET=1 keeps one spare prefix per node, the smallest warm
// pool that still avoids an ENI call on every pod start.
var vpcCNIConfig = map[string]any{
	"env": map[string]any{
		"ENABLE_PREFIX_DELEGATION": "true",
		"WARM_PREFIX_TARGET":       "1",
	},
}

// networkAddons must exist before any node group: a managed node group
// works out its nodes' max-pods from the VPC CNI configuration when the
// group is created, so a group created first keeps the ENI-bound limit
// until its nodes are replaced. The VPC CNI keeps using the node role's
// AmazonEKS_CNI_Policy rather than Pod Identity: it has to hand out pod IPs
// before the Pod Identity agent itself can start.
func networkAddons() []managedAddon {
	return []managedAddon{
		{name: addonVPCCNI, config: vpcCNIConfig},
		{name: addonKubeProxy},
	}
}

// workloadAddons are installed once node groups exist, in this order: the
// Pod Identity agent first, since every later add-on with an identity gets
// its credentials through it.
func workloadAddons(spec core.ClusterSpec) []managedAddon {
	n := names{spec}
	return []managedAddon{
		{name: addonPodIdentityAgent},
		{name: addonCoreDNS},
		{
			name: addonEBSCSIDriver,
			identity: &addonIdentity{
				serviceAccount:  "ebs-csi-controller-sa",
				role:            n.ebsCSIRole(),
				managedPolicies: []string{policyEBSCSIDriver},
			},
		},
		{
			name: addonEFSCSIDriver,
			identity: &addonIdentity{
				serviceAccount:  "efs-csi-controller-sa",
				role:            n.efsCSIRole(),
				managedPolicies: []string{policyEFSCSIDriver},
			},
			autoMode: true,
		},
		{name: addonCertManager},
		{
			name: addonExternalDNS,
			// txtOwnerId marks the records this cluster owns, so two clusters
			// sharing a hosted zone never delete each other's.
			config: map[string]any{"txtOwnerId": spec.ID.String()},
			identity: &addonIdentity{
				serviceAccount: "external-dns",
				role:           n.externalDNSRole(),
				inlinePolicy:   externalDNSPolicy(),
			},
		},
		{name: addonFluentBit},
	}
}

// identityRoles lists every role the add-ons above get, for teardown.
func identityRoles(spec core.ClusterSpec) []string {
	var roles []string
	for _, a := range workloadAddons(spec) {
		if a.identity != nil {
			roles = append(roles, a.identity.role)
		}
	}
	return roles
}

// ensureManagedAddons installs or converges each add-on in turn, skipping
// the ones Auto Mode provides itself.
func (p *ClusterProvisioner) ensureManagedAddons(
	ctx context.Context, spec core.ClusterSpec, addons []managedAddon, autoMode bool, change *provisioner.Change,
) error {
	for _, a := range addons {
		if autoMode && !a.autoMode {
			continue
		}
		if err := p.ensureManagedAddon(ctx, spec, a, change); err != nil {
			return err
		}
	}
	return nil
}

// ensureManagedAddon installs the add-on if absent, or converges its
// configuration and Pod Identity binding if it drifted. An unchanged add-on
// costs one DescribeAddon (plus the role reads, when it has an identity) and
// no writes.
//
// ResolveConflicts=OVERWRITE is what lets it adopt what is already there:
// the self-managed vpc-cni/kube-proxy/coredns EKS installs on every new
// cluster, and the Helm releases an older kubespin delivered through Argo CD.
//
// An add-on an older kubespin bound through IRSA (a service account role ARN)
// keeps it: the role still works, and moving it to Pod Identity is a
// separate, deliberate step rather than something every apply retries.
func (p *ClusterProvisioner) ensureManagedAddon(
	ctx context.Context, spec core.ClusterSpec, a managedAddon, change *provisioner.Change,
) error {
	var config string
	if a.config != nil {
		raw, err := json.Marshal(a.config)
		if err != nil {
			return fmt.Errorf("rendering %s configuration: %w", a.name, err)
		}
		config = string(raw)
	}

	var association []ekstypes.AddonPodIdentityAssociations
	if a.identity != nil {
		roleARN, err := p.ensureRole(ctx, a.identity.role, podIdentityTrust(), a.identity.managedPolicies)
		if err != nil {
			return fmt.Errorf("ensuring role for %s: %w", a.name, err)
		}
		if a.identity.inlinePolicy != nil {
			if err := p.ensureInlinePolicy(ctx, a.identity.role, a.name, a.identity.inlinePolicy); err != nil {
				return err
			}
		}
		association = []ekstypes.AddonPodIdentityAssociations{{
			RoleArn:        aws.String(roleARN),
			ServiceAccount: aws.String(a.identity.serviceAccount),
		}}
	}

	clusterName := names{spec}.cluster()
	desc, err := p.c.eks.DescribeAddon(ctx, &eks.DescribeAddonInput{
		ClusterName: aws.String(clusterName),
		AddonName:   aws.String(a.name),
	})
	if err == nil {
		if desc.Addon == nil {
			return nil
		}
		usesIRSA := aws.ToString(desc.Addon.ServiceAccountRoleArn) != ""
		identityMissing := association != nil && !usesIRSA && len(desc.Addon.PodIdentityAssociations) == 0
		configDrifted := config != "" && !sameJSON(aws.ToString(desc.Addon.ConfigurationValues), config)
		if !identityMissing && !configDrifted {
			return nil
		}

		input := &eks.UpdateAddonInput{
			ClusterName:      aws.String(clusterName),
			AddonName:        aws.String(a.name),
			ResolveConflicts: ekstypes.ResolveConflictsOverwrite,
		}
		if identityMissing {
			input.PodIdentityAssociations = association
		}
		if configDrifted {
			input.ConfigurationValues = aws.String(config)
		}
		if _, err := p.c.eks.UpdateAddon(ctx, input); err != nil {
			return fmt.Errorf("updating addon %s for %s: %w", a.name, spec.ID, err)
		}
		p.c.logger.Info("Updated EKS Addon", "cluster", spec.ID, "addon", a.name,
			"podIdentity", identityMissing, "configuration", configDrifted)
		record(change, "update addon "+a.name)
		return nil
	}

	var missing *ekstypes.ResourceNotFoundException
	if !errors.As(err, &missing) {
		return fmt.Errorf("describing addon %s for %s: %w", a.name, spec.ID, err)
	}

	input := &eks.CreateAddonInput{
		ClusterName:             aws.String(clusterName),
		AddonName:               aws.String(a.name),
		ResolveConflicts:        ekstypes.ResolveConflictsOverwrite,
		PodIdentityAssociations: association,
		Tags:                    tags(spec),
	}
	if config != "" {
		input.ConfigurationValues = aws.String(config)
	}
	if _, err := p.c.eks.CreateAddon(ctx, input); err != nil {
		var exists *ekstypes.ResourceInUseException
		if errors.As(err, &exists) {
			return nil
		}
		return fmt.Errorf("creating addon %s for %s: %w", a.name, spec.ID, err)
	}
	p.c.logger.Info("Installed EKS Addon", "cluster", spec.ID, "addon", a.name)
	record(change, "install addon "+a.name)
	return nil
}

// externalDNSPolicy lets external-dns manage records in the account's
// hosted zones. It is narrower than the AmazonRoute53FullAccess policy the
// add-on recommends: writes only to record sets, no zone creation or
// deletion. Which zones it touches is external-dns's own domainFilters
// configuration.
func externalDNSPolicy() map[string]any {
	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{
			map[string]any{
				"Effect":   "Allow",
				"Action":   []string{"route53:ChangeResourceRecordSets"},
				"Resource": "arn:aws:route53:::hostedzone/*",
			},
			map[string]any{
				"Effect": "Allow",
				"Action": []string{
					"route53:ListHostedZones",
					"route53:ListResourceRecordSets",
					"route53:ListTagsForResources",
				},
				"Resource": "*",
			},
		},
	}
}
