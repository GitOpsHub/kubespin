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

	clusterPolicies := []string{policyEKSCluster}
	clusterTrust := eksServiceTrust("eks.amazonaws.com")
	if spec.Autopilot {
		clusterPolicies = append(clusterPolicies,
			policyEKSComputePolicy, policyEKSBlockStoragePolicy,
			policyEKSLoadBalancingPolicy, policyEKSNetworkingPolicy)
		// Auto Mode's tag-based resource scoping requires the cluster role to
		// also be able to tag the sessions it assumes under, beyond the plain
		// AssumeRole every EKS cluster role needs.
		clusterTrust = eksClusterAutoModeTrust("eks.amazonaws.com")
	}
	clusterRoleARN, err := p.ensureRole(ctx, names{spec}.clusterRole(), clusterTrust, clusterPolicies)
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
		return p.ensureComputeAndAddons(ctx, spec, state, nil)
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
	if spec.Autopilot {
		autoNodeRoleARN, err := p.ensureRole(ctx, names{spec}.autoNodeRole(),
			eksServiceTrust("ec2.amazonaws.com"), []string{policyEKSWorkerNodeMinimal, policyECRPullOnly})
		if err != nil {
			return fmt.Errorf("ensuring Auto Mode node role for %s: %w", spec.ID, err)
		}
		in.AccessConfig = &ekstypes.CreateAccessConfigRequest{
			AuthenticationMode:                      ekstypes.AuthenticationModeApiAndConfigMap,
			BootstrapClusterCreatorAdminPermissions: aws.Bool(true),
		}
		in.ComputeConfig = &ekstypes.ComputeConfigRequest{
			Enabled:     aws.Bool(true),
			NodePools:   []string{"general-purpose", "system"},
			NodeRoleArn: aws.String(autoNodeRoleARN),
		}
		in.KubernetesNetworkConfig = &ekstypes.KubernetesNetworkConfigRequest{
			ElasticLoadBalancing: &ekstypes.ElasticLoadBalancing{Enabled: aws.Bool(true)},
		}
		in.StorageConfig = &ekstypes.StorageConfigRequest{
			BlockStorage: &ekstypes.BlockStorage{Enabled: aws.Bool(true)},
		}
	}

	if _, err := p.c.eks.CreateCluster(ctx, in); err != nil {
		// Another run got there first; that is convergence, not failure.
		var exists *ekstypes.ResourceInUseException
		if errors.As(err, &exists) {
			p.c.logger.Debug("EKS Cluster Already Exists", "cluster", spec.ID)
			return nil
		}
		return fmt.Errorf("creating EKS cluster %s: %w", spec.ID, err)
	}
	p.c.logger.Info("Requested EKS Cluster", "cluster", spec.ID, "region", spec.Region)
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
		Status:    normaliseStatus(cluster.Status),
		Endpoint:  aws.ToString(cluster.Endpoint),
		Version:   aws.ToString(cluster.Version),
		Access:    accessFrom(cluster.ResourcesVpcConfig),
		Autopilot: cluster.ComputeConfig != nil && aws.ToBool(cluster.ComputeConfig.Enabled),
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

	if err := p.ensureComputeAndAddons(ctx, spec, state, &change); err != nil {
		return change, err
	}
	return change, nil
}

// ensureComputeAndAddons converges everything that runs on an active
// cluster, in dependency order: the network add-ons (their configuration
// fixes a node group's max-pods at creation), the node groups, the add-ons
// that need nodes, then the cluster-autoscaler identity. Auto Mode manages
// its own nodes and scaling, so it only gets the add-ons it lacks.
func (p *ClusterProvisioner) ensureComputeAndAddons(
	ctx context.Context, spec core.ClusterSpec, state provisioner.ClusterState, change *provisioner.Change,
) error {
	if err := p.ensureManagedAddons(ctx, spec, networkAddons(), state.Autopilot, change); err != nil {
		return err
	}
	if !state.Autopilot {
		if err := p.ensureNodeGroups(ctx, spec, change); err != nil {
			return err
		}
	}
	if err := p.ensureManagedAddons(ctx, spec, workloadAddons(spec), state.Autopilot, change); err != nil {
		return err
	}
	if state.Autopilot {
		return nil
	}
	return p.ensureClusterAutoscalerIdentity(ctx, spec, change)
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
	p.c.logger.Info("Updated Access Mode", "cluster", spec.ID, "from", state.Access, "to", spec.Access)

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
			p.c.logger.Info("Created Node Pool", "cluster", spec.ID, "pool", want.Name)
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
		p.c.logger.Info("Resized Node Pool", "cluster", spec.ID, "pool", want.Name,
			"min", want.MinSize, "desired", want.DesiredSize, "max", want.MaxSize)
		record(change, fmt.Sprintf("resize node pool %s to %d/%d/%d",
			want.Name, want.MinSize, want.DesiredSize, want.MaxSize))
	}

	return nil
}

func (p *ClusterProvisioner) createNodeGroup(
	ctx context.Context, spec core.ClusterSpec, pool core.NodePool, nodeRoleARN string,
) error {
	instanceTypes := []string{pool.InstanceType}
	if pool.InstanceType == core.InstanceTypeAuto {
		resolved, err := p.resolveAutoInstanceTypes(ctx, spec)
		if err != nil {
			return fmt.Errorf("choosing instance types for node pool %s: %w", pool.Name, err)
		}
		instanceTypes = resolved
	}

	input := &eks.CreateNodegroupInput{
		ClusterName:   aws.String(names{spec}.cluster()),
		NodegroupName: aws.String(names{spec}.nodeGroup(pool.Name)),
		NodeRole:      aws.String(nodeRoleARN),
		Subnets:       spec.Subnets,
		InstanceTypes: instanceTypes,
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
		// instance type using price-capacity-optimized allocation, so with
		// core.InstanceTypeAuto's list of the cheapest types each launch
		// lands on the lowest-priced pool that still has capacity.
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
// node groups, the cluster itself, and the IAM roles ensureRole created
// (cluster, node, and each add-on's Pod Identity role). EKS removes a
// cluster's Pod Identity associations with it, but none of the IAM roles —
// DeleteCluster removes only the cluster resource. A cluster created by an
// older, IRSA-based kubespin also has an IAM OIDC provider, removed here
// too.
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
			p.c.logger.Info("Deleting Node Groups", "cluster", spec.ID, "count", len(listed.Nodegroups))

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
			p.c.logger.Info("Requested EKS Cluster Deletion", "cluster", spec.ID)
		}
	}

	if err := p.deleteRole(ctx, names{spec}.clusterRole()); err != nil {
		return err
	}
	if err := p.deleteRole(ctx, names{spec}.autoNodeRole()); err != nil {
		return err
	}
	for _, role := range append(identityRoles(spec), names{spec}.clusterAutoscalerRole()) {
		if err := p.deleteRole(ctx, role); err != nil {
			return err
		}
	}
	if state.OIDCIssuer != "" {
		if err := p.deleteOIDCProvider(ctx, state.OIDCIssuer); err != nil {
			return err
		}
	}
	return nil
}

// deleteRole detaches every attached policy, deletes every inline one, and removes the role from every
// instance profile it belongs to, then deletes the role. IAM refuses to
// delete a role that still has policies attached or is still in an instance
// profile, so an orphaned role would survive teardown if either step were
// skipped. The instance-profile case matters for the EKS Auto Mode
// node role: EKS itself creates and attaches an instance profile for it
// (kubespin never calls CreateInstanceProfile), so teardown has to find and
// detach that profile rather than assuming it owns every attachment.
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

	inline, err := p.c.iam.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: aws.String(name)})
	if err != nil {
		return fmt.Errorf("listing inline policies on %s: %w", name, err)
	}
	for _, policy := range inline.PolicyNames {
		if _, err := p.c.iam.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{
			RoleName:   aws.String(name),
			PolicyName: aws.String(policy),
		}); err != nil {
			return fmt.Errorf("deleting inline policy %s from %s: %w", policy, name, err)
		}
	}

	profiles, err := p.c.iam.ListInstanceProfilesForRole(ctx, &iam.ListInstanceProfilesForRoleInput{
		RoleName: aws.String(name),
	})
	if err != nil {
		var missing *iamtypes.NoSuchEntityException
		if !errors.As(err, &missing) {
			return fmt.Errorf("listing instance profiles for %s: %w", name, err)
		}
		profiles = &iam.ListInstanceProfilesForRoleOutput{}
	}
	for _, profile := range profiles.InstanceProfiles {
		if _, err := p.c.iam.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{
			InstanceProfileName: profile.InstanceProfileName,
			RoleName:            aws.String(name),
		}); err != nil {
			return fmt.Errorf("removing %s from instance profile %s: %w", name, aws.ToString(profile.InstanceProfileName), err)
		}
	}

	if _, err := p.c.iam.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(name)}); err != nil {
		var missing *iamtypes.NoSuchEntityException
		if errors.As(err, &missing) {
			return nil
		}
		return fmt.Errorf("deleting role %s: %w", name, err)
	}
	p.c.logger.Info("Deleted IAM Role", "role", name)
	return nil
}

// deleteOIDCProvider removes the IAM OIDC provider an older, IRSA-based
// kubespin registered for the cluster, found by issuer host (nothing
// persisted its ARN). Clusters created since the move to Pod Identity have
// none, which makes this a no-op.
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
		p.c.logger.Info("Deleted OIDC Provider", "issuer", issuer)
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
	p.c.logger.Info("Created IAM Role", "role", name)
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

// eksClusterAutoModeTrust is eksServiceTrust plus sts:TagSession, which EKS
// Auto Mode's cluster role needs for its tag-based resource scoping
// (docs.aws.amazon.com/eks/latest/userguide/auto-cluster-iam-role.html).
func eksClusterAutoModeTrust(service string) map[string]any {
	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect":    "Allow",
			"Action":    []string{"sts:AssumeRole", "sts:TagSession"},
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
