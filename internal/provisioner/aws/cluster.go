package aws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

// ClusterProvisioner creates and reconciles EKS clusters.
type ClusterProvisioner struct {
	c *Clients

	// wait tunes the polls Delete makes while node groups drain.
	wait provisioner.WaitOptions
}

// NewClusterProvisioner builds an EKS provisioner over the given clients.
func NewClusterProvisioner(c *Clients) *ClusterProvisioner {
	return &ClusterProvisioner{c: c, wait: provisioner.DefaultWaitOptions()}
}

// Provider identifies this implementation's cloud.
func (p *ClusterProvisioner) Provider() core.Provider { return core.ProviderAWS }

// Create requests a cluster and its node groups.
//
// It is idempotent at every step: an existing cluster or node group is left
// alone rather than treated as an error, so a resumed run passes straight
// through to whatever is still missing.
func (p *ClusterProvisioner) Create(ctx context.Context, spec core.ClusterSpec) error {
	if err := validateForEKS(spec); err != nil {
		return err
	}

	clusterRoleARN, err := p.ensureRole(ctx, names{spec}.clusterRole(),
		eksServiceTrust("eks.amazonaws.com"), []string{policyEKSCluster})
	if err != nil {
		return err
	}

	state, err := p.Describe(ctx, spec)
	if err != nil {
		return err
	}

	if state.Status == provisioner.StatusAbsent {
		if err := p.createCluster(ctx, spec, clusterRoleARN); err != nil {
			return err
		}
		// Node groups cannot be attached until the control plane is active, so
		// they are created by Reconcile once the caller has waited.
		return nil
	}

	if state.Status == provisioner.StatusActive {
		if err := p.ensureNodeGroups(ctx, spec, nil); err != nil {
			return err
		}
		return p.ensureCSIAddons(ctx, spec, state, nil)
	}
	return nil
}

func (p *ClusterProvisioner) createCluster(ctx context.Context, spec core.ClusterSpec, roleARN string) error {
	in := &eks.CreateClusterInput{
		Name:               aws.String(names{spec}.cluster()),
		RoleArn:            aws.String(roleARN),
		ResourcesVpcConfig: vpcConfig(spec),
		Tags:               tags(spec),
	}
	if spec.KubernetesVersion != "" {
		in.Version = aws.String(spec.KubernetesVersion)
	}

	if _, err := p.c.eks.CreateCluster(ctx, in); err != nil {
		// Another run got there first; that is convergence, not failure.
		var exists *ekstypes.ResourceInUseException
		if errors.As(err, &exists) {
			p.c.logger.Debug("EKS cluster already exists", "cluster", spec.ID)
			return nil
		}
		return fmt.Errorf("creating EKS cluster %s: %w", spec.ID, err)
	}
	p.c.logger.Info("requested EKS cluster", "cluster", spec.ID, "region", spec.Region)
	return nil
}

// vpcConfig translates the access mode into EKS endpoint configuration.
//
// A private cluster has no public endpoint at all; a public one is reachable
// but still restricted to the authorized CIDRs when any are given. Both keep
// the private endpoint enabled so in-VPC traffic never leaves the network.
func vpcConfig(spec core.ClusterSpec) *ekstypes.VpcConfigRequest {
	cfg := &ekstypes.VpcConfigRequest{
		SubnetIds:             spec.Subnets,
		EndpointPrivateAccess: aws.Bool(true),
		EndpointPublicAccess:  aws.Bool(spec.Access == core.AccessPublic),
	}
	if spec.Access == core.AccessPublic && len(spec.AuthorizedCIDRs) > 0 {
		cfg.PublicAccessCidrs = spec.AuthorizedCIDRs
	}
	return cfg
}

// Describe reports the cluster's current state.
func (p *ClusterProvisioner) Describe(ctx context.Context, spec core.ClusterSpec) (provisioner.ClusterState, error) {
	out, err := p.c.eks.DescribeCluster(ctx, &eks.DescribeClusterInput{
		Name: aws.String(names{spec}.cluster()),
	})
	if err != nil {
		var missing *ekstypes.ResourceNotFoundException
		if errors.As(err, &missing) {
			// Absent is a normal answer while polling, not an error.
			return provisioner.ClusterState{Status: provisioner.StatusAbsent}, nil
		}
		return provisioner.ClusterState{}, fmt.Errorf("describing EKS cluster %s: %w", spec.ID, err)
	}

	cluster := out.Cluster
	if cluster == nil {
		return provisioner.ClusterState{Status: provisioner.StatusAbsent}, nil
	}

	state := provisioner.ClusterState{
		Status:   normaliseStatus(cluster.Status),
		Endpoint: aws.ToString(cluster.Endpoint),
		Version:  aws.ToString(cluster.Version),
		Access:   accessFrom(cluster.ResourcesVpcConfig),
	}
	if cluster.Identity != nil && cluster.Identity.Oidc != nil {
		state.OIDCIssuer = aws.ToString(cluster.Identity.Oidc.Issuer)
	}
	if cluster.ResourcesVpcConfig != nil {
		state.NetworkID = aws.ToString(cluster.ResourcesVpcConfig.ClusterSecurityGroupId)
	}
	if cluster.CertificateAuthority != nil {
		if ca, err := base64.StdEncoding.DecodeString(aws.ToString(cluster.CertificateAuthority.Data)); err == nil {
			state.CertificateAuthorityData = ca
		}
	}

	if state.Status == provisioner.StatusActive {
		pools, err := p.describeNodePools(ctx, spec)
		if err != nil {
			return state, err
		}
		state.NodePools = pools
	}

	return state, nil
}

func normaliseStatus(status ekstypes.ClusterStatus) provisioner.Status {
	switch status {
	case ekstypes.ClusterStatusActive:
		return provisioner.StatusActive
	case ekstypes.ClusterStatusCreating:
		return provisioner.StatusCreating
	case ekstypes.ClusterStatusUpdating:
		return provisioner.StatusUpdating
	case ekstypes.ClusterStatusDeleting:
		return provisioner.StatusDeleting
	case ekstypes.ClusterStatusFailed, ekstypes.ClusterStatusPending:
		// Pending is grouped with failed deliberately: EKS reports it for a
		// cluster that could not start, and waiting will not clear it.
		if status == ekstypes.ClusterStatusPending {
			return provisioner.StatusCreating
		}
		return provisioner.StatusFailed
	default:
		return provisioner.StatusFailed
	}
}

func accessFrom(cfg *ekstypes.VpcConfigResponse) core.Access {
	if cfg != nil && cfg.EndpointPublicAccess {
		return core.AccessPublic
	}
	return core.AccessPrivate
}

func (p *ClusterProvisioner) describeNodePools(ctx context.Context, spec core.ClusterSpec) ([]core.NodePool, error) {
	listed, err := p.c.eks.ListNodegroups(ctx, &eks.ListNodegroupsInput{
		ClusterName: aws.String(names{spec}.cluster()),
	})
	if err != nil {
		return nil, fmt.Errorf("listing node groups for %s: %w", spec.ID, err)
	}

	pools := make([]core.NodePool, 0, len(listed.Nodegroups))
	for _, name := range listed.Nodegroups {
		out, err := p.c.eks.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{
			ClusterName:   aws.String(names{spec}.cluster()),
			NodegroupName: aws.String(name),
		})
		if err != nil {
			return nil, fmt.Errorf("describing node group %s: %w", name, err)
		}
		if out.Nodegroup == nil {
			continue
		}

		pool := core.NodePool{
			Name:       poolNameFromNodeGroup(spec, name),
			Labels:     out.Nodegroup.Labels,
			DiskSizeGB: aws.ToInt32(out.Nodegroup.DiskSize),
		}
		if types := out.Nodegroup.InstanceTypes; len(types) > 0 {
			pool.InstanceType = types[0]
		}
		if scaling := out.Nodegroup.ScalingConfig; scaling != nil {
			pool.MinSize = aws.ToInt32(scaling.MinSize)
			pool.MaxSize = aws.ToInt32(scaling.MaxSize)
			pool.DesiredSize = aws.ToInt32(scaling.DesiredSize)
		}
		pools = append(pools, pool)
	}

	slices.SortFunc(pools, func(a, b core.NodePool) int { return strings.Compare(a.Name, b.Name) })
	return pools, nil
}

func poolNameFromNodeGroup(spec core.ClusterSpec, nodeGroup string) string {
	return strings.TrimPrefix(nodeGroup, spec.ID.String()+"-")
}

// Reconcile brings an existing cluster in line with the spec.
//
// It reports whether it changed anything as data. `apply` proves it made no
// cloud calls when nothing differs, and that cannot be inferred by diffing
// state before and after.
func (p *ClusterProvisioner) Reconcile(ctx context.Context, spec core.ClusterSpec) (provisioner.Change, error) {
	var change provisioner.Change

	state, err := p.Describe(ctx, spec)
	if err != nil {
		return change, err
	}
	if state.Status == provisioner.StatusAbsent {
		return change, fmt.Errorf("%w: %s", provisioner.ErrNotFound, spec.ID)
	}

	accessChange, err := p.reconcileAccess(ctx, spec, state)
	if err != nil {
		return change, err
	}
	change.Merge(accessChange)

	if err := p.ensureNodeGroups(ctx, spec, &change); err != nil {
		return change, err
	}
	if err := p.ensureCSIAddons(ctx, spec, state, &change); err != nil {
		return change, err
	}
	return change, nil
}

// ensureCSIAddons installs the EBS and EFS CSI drivers as EKS-managed
// addons (not Helm charts): EKS owns their lifecycle once requested, so this
// only has to provision the IRSA role each one assumes and request/update
// the addon by name — the same division of labor `eksctl create addon` uses.
//
// Both are AWS-only by construction: they are EKS addon names, so there is
// nothing to gate by provider the way catalog addons gate karpenter.
func (p *ClusterProvisioner) ensureCSIAddons(
	ctx context.Context, spec core.ClusterSpec, state provisioner.ClusterState, change *provisioner.Change,
) error {
	if state.OIDCIssuer == "" {
		return fmt.Errorf("cluster %s reports no OIDC issuer", spec.ID)
	}

	idp := NewIdentityProvisioner(p.c)
	providerARN, err := idp.ensureOIDCProvider(ctx, state.OIDCIssuer)
	if err != nil {
		return err
	}

	for _, d := range []struct {
		addonName string
		roleName  string
		policy    string
		comp      provisioner.Component
	}{
		{
			addonName: addonEBSCSIDriver,
			roleName:  names{spec}.ebsCSIRole(),
			policy:    policyEBSCSIDriver,
			comp:      provisioner.Component{Name: "ebs-csi", Namespace: "kube-system", ServiceAccount: "ebs-csi-controller-sa"},
		},
		{
			addonName: addonEFSCSIDriver,
			roleName:  names{spec}.efsCSIRole(),
			policy:    policyEFSCSIDriver,
			comp:      provisioner.Component{Name: "efs-csi", Namespace: "kube-system", ServiceAccount: "efs-csi-controller-sa"},
		},
	} {
		trust := irsaTrustPolicy(providerARN, state.OIDCIssuer, d.comp)
		roleARN, err := p.ensureRole(ctx, d.roleName, trust, []string{d.policy})
		if err != nil {
			return fmt.Errorf("ensuring role for %s: %w", d.addonName, err)
		}

		installed, err := p.ensureAddon(ctx, spec, d.addonName, roleARN)
		if err != nil {
			return err
		}
		if installed {
			record(change, fmt.Sprintf("install addon %s", d.addonName))
		}
	}
	return nil
}

// ensureAddon requests the named EKS-managed addon if absent, or converges
// its IRSA role if it already exists and drifted. It reports whether it
// created the addon, so callers collecting a Change only see something real.
func (p *ClusterProvisioner) ensureAddon(
	ctx context.Context, spec core.ClusterSpec, addonName, roleARN string,
) (bool, error) {
	clusterName := names{spec}.cluster()

	desc, err := p.c.eks.DescribeAddon(ctx, &eks.DescribeAddonInput{
		ClusterName: aws.String(clusterName),
		AddonName:   aws.String(addonName),
	})
	if err == nil {
		if desc.Addon != nil && aws.ToString(desc.Addon.ServiceAccountRoleArn) != roleARN {
			if _, err := p.c.eks.UpdateAddon(ctx, &eks.UpdateAddonInput{
				ClusterName:           aws.String(clusterName),
				AddonName:             aws.String(addonName),
				ServiceAccountRoleArn: aws.String(roleARN),
				ResolveConflicts:      ekstypes.ResolveConflictsOverwrite,
			}); err != nil {
				return false, fmt.Errorf("updating addon %s role for %s: %w", addonName, spec.ID, err)
			}
			p.c.logger.Info("updated EKS addon role", "cluster", spec.ID, "addon", addonName)
		}
		return false, nil
	}

	var missing *ekstypes.ResourceNotFoundException
	if !errors.As(err, &missing) {
		return false, fmt.Errorf("describing addon %s for %s: %w", addonName, spec.ID, err)
	}

	if _, err := p.c.eks.CreateAddon(ctx, &eks.CreateAddonInput{
		ClusterName:           aws.String(clusterName),
		AddonName:             aws.String(addonName),
		ServiceAccountRoleArn: aws.String(roleARN),
		ResolveConflicts:      ekstypes.ResolveConflictsOverwrite,
		Tags:                  tags(spec),
	}); err != nil {
		var exists *ekstypes.ResourceInUseException
		if errors.As(err, &exists) {
			return false, nil
		}
		return false, fmt.Errorf("creating addon %s for %s: %w", addonName, spec.ID, err)
	}
	p.c.logger.Info("installed EKS addon", "cluster", spec.ID, "addon", addonName)
	return true, nil
}

func (p *ClusterProvisioner) reconcileAccess(
	ctx context.Context, spec core.ClusterSpec, state provisioner.ClusterState,
) (provisioner.Change, error) {
	if state.Access == spec.Access {
		return provisioner.Change{}, nil
	}

	_, err := p.c.eks.UpdateClusterConfig(ctx, &eks.UpdateClusterConfigInput{
		Name:               aws.String(names{spec}.cluster()),
		ResourcesVpcConfig: vpcConfig(spec),
	})
	if err != nil {
		return provisioner.Change{}, fmt.Errorf("updating access mode for %s: %w", spec.ID, err)
	}
	p.c.logger.Info("updated cluster access mode", "cluster", spec.ID, "from", state.Access, "to", spec.Access)

	return provisioner.Change{
		Changed: true,
		Details: []string{fmt.Sprintf("access %s -> %s", state.Access, spec.Access)},
	}, nil
}

// ensureNodeGroups creates missing node groups and resizes drifted ones.
// It never deletes: removing a node pool evicts running workloads, which is a
// decision that belongs to a human rather than to a reconcile loop.
func (p *ClusterProvisioner) ensureNodeGroups(
	ctx context.Context, spec core.ClusterSpec, change *provisioner.Change,
) error {
	nodeRoleARN, err := p.ensureRole(ctx, names{spec}.nodeRole(),
		eksServiceTrust("ec2.amazonaws.com"),
		[]string{policyEKSWorkerNode, policyEKSCNI, policyECRReadOnly})
	if err != nil {
		return err
	}

	existing, err := p.describeNodePools(ctx, spec)
	if err != nil {
		return err
	}

	for _, want := range spec.NodePools {
		current, found := findPool(existing, want.Name)
		if !found {
			if err := p.createNodeGroup(ctx, spec, want, nodeRoleARN); err != nil {
				return err
			}
			p.c.logger.Info("created node pool", "cluster", spec.ID, "pool", want.Name)
			record(change, fmt.Sprintf("create node pool %s", want.Name))
			continue
		}

		if current.MinSize == want.MinSize && current.MaxSize == want.MaxSize &&
			current.DesiredSize == want.DesiredSize {
			continue
		}

		_, err := p.c.eks.UpdateNodegroupConfig(ctx, &eks.UpdateNodegroupConfigInput{
			ClusterName:   aws.String(names{spec}.cluster()),
			NodegroupName: aws.String(names{spec}.nodeGroup(want.Name)),
			ScalingConfig: &ekstypes.NodegroupScalingConfig{
				MinSize:     aws.Int32(want.MinSize),
				MaxSize:     aws.Int32(want.MaxSize),
				DesiredSize: aws.Int32(want.DesiredSize),
			},
		})
		if err != nil {
			return fmt.Errorf("resizing node pool %s: %w", want.Name, err)
		}
		p.c.logger.Info("resized node pool", "cluster", spec.ID, "pool", want.Name,
			"min", want.MinSize, "desired", want.DesiredSize, "max", want.MaxSize)
		record(change, fmt.Sprintf("resize node pool %s to %d/%d/%d",
			want.Name, want.MinSize, want.DesiredSize, want.MaxSize))
	}

	return nil
}

func (p *ClusterProvisioner) createNodeGroup(
	ctx context.Context, spec core.ClusterSpec, pool core.NodePool, nodeRoleARN string,
) error {
	input := &eks.CreateNodegroupInput{
		ClusterName:   aws.String(names{spec}.cluster()),
		NodegroupName: aws.String(names{spec}.nodeGroup(pool.Name)),
		NodeRole:      aws.String(nodeRoleARN),
		Subnets:       spec.Subnets,
		InstanceTypes: []string{pool.InstanceType},
		ScalingConfig: &ekstypes.NodegroupScalingConfig{
			MinSize:     aws.Int32(pool.MinSize),
			MaxSize:     aws.Int32(pool.MaxSize),
			DesiredSize: aws.Int32(pool.DesiredSize),
		},
		Labels: pool.Labels,
		Tags:   tags(spec),
	}
	if pool.DiskSizeGB > 0 {
		input.DiskSize = aws.Int32(pool.DiskSizeGB)
	}
	if pool.CapacityType == core.CapacityTypeSpot {
		// EKS spot node groups draw from the Spot pools of every listed
		// instance type; a single type (as kubespin passes today) still
		// works, it just has less allocation flexibility than a list would.
		input.CapacityType = ekstypes.CapacityTypesSpot
	}
	_, err := p.c.eks.CreateNodegroup(ctx, input)
	if err != nil {
		var exists *ekstypes.ResourceInUseException
		if errors.As(err, &exists) {
			return nil
		}
		return fmt.Errorf("creating node pool %s: %w", pool.Name, err)
	}
	return nil
}

// Delete tears down node groups then the cluster.
//
// It returns once EKS has accepted the cluster deletion; the caller polls
// Describe (provisioner.WaitUntilGone) until the cluster is really gone. A
// cluster already tearing down is convergence rather than an error, so a
// retried teardown resumes instead of failing on ResourceInUseException.
// Delete tears down the cluster and everything Create provisioned around it:
// node groups, the cluster itself, the clusterRole/nodeRole IAM roles
// ensureRole created, and the IAM OIDC provider ensureOIDCProvider
// registered. None of those IAM resources are cleaned up by EKS on its
// own — DeleteCluster removes only the cluster resource — so skipping this
// left every deleted cluster's roles and OIDC provider behind indefinitely
// before this existed.
//
// The role/OIDC cleanup at the end runs unconditionally, including on the
// early-return paths below, so a retried delete against a cluster already
// deleting (or already gone) from an earlier, interrupted run still reaches
// it — every call here is independently idempotent (NoSuchEntityException on
// an already-deleted role or provider is treated as success), so nothing
// about calling it again is unsafe. The one gap this cannot close: if a
// cluster finishes deleting entirely between one Delete call and the next,
// Describe can no longer report its OIDC issuer (EKS does not keep it around
// once the cluster is gone), so a delete resumed only after that point cannot
// find the OIDC provider by issuer host and it is left behind — narrow, since
// deletion takes minutes, but real.
func (p *ClusterProvisioner) Delete(ctx context.Context, spec core.ClusterSpec) error {
	state, err := p.Describe(ctx, spec)
	if err != nil {
		return err
	}

	if state.Status != provisioner.StatusAbsent && state.Status != provisioner.StatusDeleting {
		listed, err := p.c.eks.ListNodegroups(ctx, &eks.ListNodegroupsInput{
			ClusterName: aws.String(names{spec}.cluster()),
		})
		if err != nil {
			var missing *ekstypes.ResourceNotFoundException
			if !errors.As(err, &missing) {
				return fmt.Errorf("listing node groups for %s: %w", spec.ID, err)
			}
		}

		if listed != nil && len(listed.Nodegroups) > 0 {
			p.c.logger.Info("deleting node groups before cluster", "cluster", spec.ID, "count", len(listed.Nodegroups))

			// Node groups must go first: EKS refuses to delete a cluster that
			// still has any attached.
			for _, name := range listed.Nodegroups {
				_, err := p.c.eks.DeleteNodegroup(ctx, &eks.DeleteNodegroupInput{
					ClusterName:   aws.String(names{spec}.cluster()),
					NodegroupName: aws.String(name),
				})
				if err != nil {
					var missing *ekstypes.ResourceNotFoundException
					if !errors.As(err, &missing) {
						return fmt.Errorf("deleting node group %s: %w", name, err)
					}
				}
			}

			// DeleteNodegroup only accepts the request — draining and
			// terminating the nodes takes minutes, and DeleteCluster fails
			// with ResourceInUseException the whole time. Poll until the last
			// one is gone rather than racing it.
			if err := p.waitForNodeGroupsGone(ctx, spec); err != nil {
				return err
			}
		}

		// Nodes are fully terminated by this point, so the instance role they
		// ran under is no longer needed.
		if err := p.deleteRole(ctx, names{spec}.nodeRole()); err != nil {
			return err
		}

		if _, err := p.c.eks.DeleteCluster(ctx, &eks.DeleteClusterInput{
			Name: aws.String(names{spec}.cluster()),
		}); err != nil {
			var missing *ekstypes.ResourceNotFoundException
			if !errors.As(err, &missing) {
				return fmt.Errorf("deleting EKS cluster %s: %w", spec.ID, err)
			}
		} else {
			p.c.logger.Info("requested EKS cluster deletion", "cluster", spec.ID)
		}
	}

	if err := p.deleteRole(ctx, names{spec}.clusterRole()); err != nil {
		return err
	}
	if err := p.deleteRole(ctx, names{spec}.ebsCSIRole()); err != nil {
		return err
	}
	if err := p.deleteRole(ctx, names{spec}.efsCSIRole()); err != nil {
		return err
	}
	if state.OIDCIssuer != "" {
		if err := p.deleteOIDCProvider(ctx, state.OIDCIssuer); err != nil {
			return err
		}
	}
	return nil
}

// deleteRole detaches every attached policy, then deletes the role. IAM
// refuses to delete a role that still has policies attached, so an orphaned
// role would survive teardown if the detach step were skipped — the same
// reasoning IdentityProvisioner.Deprovision follows for the IRSA role.
func (p *ClusterProvisioner) deleteRole(ctx context.Context, name string) error {
	attached, err := p.c.iam.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{
		RoleName: aws.String(name),
	})
	if err != nil {
		var missing *iamtypes.NoSuchEntityException
		if errors.As(err, &missing) {
			return nil
		}
		return fmt.Errorf("listing policies on %s: %w", name, err)
	}

	for _, policy := range attached.AttachedPolicies {
		if _, err := p.c.iam.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{
			RoleName:  aws.String(name),
			PolicyArn: policy.PolicyArn,
		}); err != nil {
			return fmt.Errorf("detaching %s from %s: %w", aws.ToString(policy.PolicyArn), name, err)
		}
	}

	if _, err := p.c.iam.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(name)}); err != nil {
		var missing *iamtypes.NoSuchEntityException
		if errors.As(err, &missing) {
			return nil
		}
		return fmt.Errorf("deleting role %s: %w", name, err)
	}
	p.c.logger.Info("deleted IAM role", "role", name)
	return nil
}

// deleteOIDCProvider removes the cluster's IAM OIDC provider, found the same
// way ensureOIDCProvider looked it up: by issuer host, since IAM has no
// lookup-by-ARN-we-remember (nothing about this package persists the ARN
// ensureOIDCProvider returned at creation time).
func (p *ClusterProvisioner) deleteOIDCProvider(ctx context.Context, issuer string) error {
	host := strings.TrimPrefix(issuer, "https://")

	listed, err := p.c.iam.ListOpenIDConnectProviders(ctx, &iam.ListOpenIDConnectProvidersInput{})
	if err != nil {
		return fmt.Errorf("listing OIDC providers: %w", err)
	}

	for _, entry := range listed.OpenIDConnectProviderList {
		arn := aws.ToString(entry.Arn)

		got, err := p.c.iam.GetOpenIDConnectProvider(ctx, &iam.GetOpenIDConnectProviderInput{
			OpenIDConnectProviderArn: aws.String(arn),
		})
		if err != nil {
			var missing *iamtypes.NoSuchEntityException
			if errors.As(err, &missing) {
				continue
			}
			return fmt.Errorf("describing OIDC provider %s: %w", arn, err)
		}
		if aws.ToString(got.Url) != host {
			continue
		}

		if _, err := p.c.iam.DeleteOpenIDConnectProvider(ctx, &iam.DeleteOpenIDConnectProviderInput{
			OpenIDConnectProviderArn: aws.String(arn),
		}); err != nil {
			var missing *iamtypes.NoSuchEntityException
			if errors.As(err, &missing) {
				return nil
			}
			return fmt.Errorf("deleting OIDC provider %s: %w", arn, err)
		}
		p.c.logger.Info("deleted OIDC provider", "issuer", issuer)
		return nil
	}
	return nil
}

// waitForNodeGroupsGone polls until the cluster reports no node groups.
//
// A vanished cluster is success, not an error: something else finished the
// teardown, which is exactly what this was waiting for.
func (p *ClusterProvisioner) waitForNodeGroupsGone(ctx context.Context, spec core.ClusterSpec) error {
	opts := p.wait
	if opts.Interval <= 0 {
		opts.Interval = provisioner.DefaultWaitOptions().Interval
	}
	if opts.Timeout <= 0 {
		opts.Timeout = provisioner.DefaultWaitOptions().Timeout
	}
	deadline := time.Now().Add(opts.Timeout)

	for {
		listed, err := p.c.eks.ListNodegroups(ctx, &eks.ListNodegroupsInput{
			ClusterName: aws.String(names{spec}.cluster()),
		})
		if err != nil {
			var missing *ekstypes.ResourceNotFoundException
			if errors.As(err, &missing) {
				return nil
			}
			return fmt.Errorf("listing node groups for %s: %w", spec.ID, err)
		}
		if len(listed.Nodegroups) == 0 {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for node groups of %s to delete; still present: %s",
				spec.ID, strings.Join(listed.Nodegroups, ", "))
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for node groups of %s to delete: %w", spec.ID, ctx.Err())
		case <-time.After(opts.Interval):
		}
	}
}

// ensureRole creates a service role if absent and attaches the managed policies.
func (p *ClusterProvisioner) ensureRole(
	ctx context.Context, name string, trust map[string]any, policies []string,
) (string, error) {
	out, err := p.c.iam.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	if err == nil {
		if err := p.attachPolicies(ctx, name, policies); err != nil {
			return "", err
		}
		return aws.ToString(out.Role.Arn), nil
	}

	var missing *iamtypes.NoSuchEntityException
	if !errors.As(err, &missing) {
		return "", fmt.Errorf("getting role %s: %w", name, err)
	}

	doc, err := json.Marshal(trust)
	if err != nil {
		return "", fmt.Errorf("rendering trust policy for %s: %w", name, err)
	}

	created, err := p.c.iam.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String(name),
		AssumeRolePolicyDocument: aws.String(string(doc)),
		Description:              aws.String("kubespin-managed role"),
	})
	if err != nil {
		return "", fmt.Errorf("creating role %s: %w", name, err)
	}
	p.c.logger.Info("created IAM role", "role", name)
	if err := p.attachPolicies(ctx, name, policies); err != nil {
		return "", err
	}
	return aws.ToString(created.Role.Arn), nil
}

func (p *ClusterProvisioner) attachPolicies(ctx context.Context, role string, policies []string) error {
	attached, err := p.c.iam.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{
		RoleName: aws.String(role),
	})
	if err != nil {
		return fmt.Errorf("listing policies on %s: %w", role, err)
	}

	have := make(map[string]struct{}, len(attached.AttachedPolicies))
	for _, policy := range attached.AttachedPolicies {
		have[aws.ToString(policy.PolicyArn)] = struct{}{}
	}

	for _, want := range policies {
		if _, ok := have[want]; ok {
			continue
		}
		if _, err := p.c.iam.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
			RoleName:  aws.String(role),
			PolicyArn: aws.String(want),
		}); err != nil {
			return fmt.Errorf("attaching %s to %s: %w", want, role, err)
		}
	}
	return nil
}

func eksServiceTrust(service string) map[string]any {
	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect":    "Allow",
			"Action":    "sts:AssumeRole",
			"Principal": map[string]any{"Service": service},
		}},
	}
}

// validateForEKS covers the requirements EKS adds beyond the shared spec rules.
func validateForEKS(spec core.ClusterSpec) error {
	// EKS places the control plane's cross-account network interfaces in at
	// least two availability zones and rejects anything less at creation time.
	if len(spec.Subnets) < 2 {
		return fmt.Errorf("%w: EKS requires at least two subnets in different availability zones, got %d",
			core.ErrInvalidSpec, len(spec.Subnets))
	}
	return nil
}

func findPool(pools []core.NodePool, name string) (core.NodePool, bool) {
	for _, pool := range pools {
		if pool.Name == name {
			return pool, true
		}
	}
	return core.NodePool{}, false
}

// record notes a change when the caller is collecting them. Create passes nil,
// because creating a cluster is not a reconcile finding.
func record(change *provisioner.Change, detail string) {
	if change == nil {
		return
	}
	change.Changed = true
	change.Details = append(change.Details, detail)
}
