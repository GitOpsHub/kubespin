package gcp

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

// ClusterProvisioner creates and reconciles GKE clusters.
type ClusterProvisioner struct {
	c *Clients
}

// NewClusterProvisioner builds a GKE provisioner over the given clients.
func NewClusterProvisioner(c *Clients) *ClusterProvisioner { return &ClusterProvisioner{c: c} }

// Provider identifies this implementation's cloud.
func (p *ClusterProvisioner) Provider() core.Provider { return core.ProviderGCP }

func (p *ClusterProvisioner) names(spec core.ClusterSpec) names {
	return names{project: p.c.project, spec: spec}
}

// Create requests a cluster and its node pools.
//
// It is idempotent at every step: an existing cluster or node pool is left
// alone rather than treated as an error, so a resumed run passes straight
// through to whatever is still missing.
func (p *ClusterProvisioner) Create(ctx context.Context, spec core.ClusterSpec) error {
	if err := validateForGKE(spec); err != nil {
		return err
	}

	state, err := p.Describe(ctx, spec)
	if err != nil {
		return err
	}

	if state.Status == provisioner.StatusAbsent {
		return p.createCluster(ctx, spec)
	}

	if state.Status == provisioner.StatusActive {
		return p.ensureNodePools(ctx, spec, nil)
	}
	return nil
}

func (p *ClusterProvisioner) createCluster(ctx context.Context, spec core.ClusterSpec) error {
	n := p.names(spec)

	cluster := &containerpb.Cluster{
		Name:                  n.cluster(),
		InitialClusterVersion: spec.KubernetesVersion,
		Subnetwork:            subnetwork(spec),
		ResourceLabels:        labels(spec),
		WorkloadIdentityConfig: &containerpb.WorkloadIdentityConfig{
			WorkloadPool: p.c.project + ".svc.id.goog",
		},
		PrivateClusterConfig:           privateClusterConfig(spec),
		MasterAuthorizedNetworksConfig: authorizedNetworksConfig(spec),
		// The default node pool is not used; node pools are created explicitly
		// once the control plane is active, mirroring the AWS provisioner.
		InitialNodeCount: 0,
		NodePools: []*containerpb.NodePool{
			placeholderNodePool(spec),
		},
	}

	_, err := p.c.cluster.CreateCluster(ctx, &containerpb.CreateClusterRequest{
		Parent:  n.parent(),
		Cluster: cluster,
	})
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			// Another run got there first; that is convergence, not failure.
			return nil
		}
		return fmt.Errorf("creating GKE cluster %s: %w", spec.ID, err)
	}
	return nil
}

// placeholderNodePool satisfies GKE's requirement that CreateCluster include
// at least one node pool. It uses the first configured pool's shape; the real
// pools (including this one, if it still needs adjusting) are reconciled by
// ensureNodePools once the control plane is active.
func placeholderNodePool(spec core.ClusterSpec) *containerpb.NodePool {
	if len(spec.NodePools) == 0 {
		return &containerpb.NodePool{Name: "default", InitialNodeCount: 1}
	}
	pool := spec.NodePools[0]
	return &containerpb.NodePool{
		Name:             pool.Name,
		InitialNodeCount: pool.DesiredSize,
		Config:           &containerpb.NodeConfig{MachineType: pool.InstanceType, Labels: pool.Labels},
		Autoscaling: &containerpb.NodePoolAutoscaling{
			Enabled:      true,
			MinNodeCount: pool.MinSize,
			MaxNodeCount: pool.MaxSize,
		},
	}
}

func subnetwork(spec core.ClusterSpec) string {
	if len(spec.Subnets) == 0 {
		return ""
	}
	return spec.Subnets[0]
}

// privateClusterConfig translates the access mode into GKE's private cluster
// settings. AccessPrivate hides even the public endpoint; AccessPublic keeps
// both endpoints, restricted by authorizedNetworksConfig when CIDRs are given.
func privateClusterConfig(spec core.ClusterSpec) *containerpb.PrivateClusterConfig {
	return &containerpb.PrivateClusterConfig{
		EnablePrivateNodes:    true,
		EnablePrivateEndpoint: spec.Access == core.AccessPrivate,
	}
}

func authorizedNetworksConfig(spec core.ClusterSpec) *containerpb.MasterAuthorizedNetworksConfig {
	if spec.Access != core.AccessPublic || len(spec.AuthorizedCIDRs) == 0 {
		return nil
	}
	blocks := make([]*containerpb.MasterAuthorizedNetworksConfig_CidrBlock, 0, len(spec.AuthorizedCIDRs))
	for _, cidr := range spec.AuthorizedCIDRs {
		blocks = append(blocks, &containerpb.MasterAuthorizedNetworksConfig_CidrBlock{CidrBlock: cidr})
	}
	return &containerpb.MasterAuthorizedNetworksConfig{Enabled: true, CidrBlocks: blocks}
}

// Describe reports the cluster's current state.
func (p *ClusterProvisioner) Describe(ctx context.Context, spec core.ClusterSpec) (provisioner.ClusterState, error) {
	n := p.names(spec)

	cluster, err := p.c.cluster.GetCluster(ctx, &containerpb.GetClusterRequest{Name: n.clusterPath()})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			// Absent is a normal answer while polling, not an error.
			return provisioner.ClusterState{Status: provisioner.StatusAbsent}, nil
		}
		return provisioner.ClusterState{}, fmt.Errorf("describing GKE cluster %s: %w", spec.ID, err)
	}

	state := provisioner.ClusterState{
		Status:    normaliseStatus(cluster.GetStatus()),
		Endpoint:  cluster.GetEndpoint(),
		Version:   cluster.GetCurrentMasterVersion(),
		Access:    accessFrom(cluster.GetPrivateClusterConfig()),
		NetworkID: cluster.GetNetwork(),
	}
	if wi := cluster.GetWorkloadIdentityConfig(); wi != nil {
		// GKE's federated OIDC issuer for a workload pool is a fixed, well
		// known URL rather than something CreateCluster returns.
		state.OIDCIssuer = "https://container.googleapis.com/v1/" + n.clusterPath()
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

func normaliseStatus(status containerpb.Cluster_Status) provisioner.Status {
	switch status {
	case containerpb.Cluster_RUNNING:
		return provisioner.StatusActive
	case containerpb.Cluster_PROVISIONING:
		return provisioner.StatusCreating
	case containerpb.Cluster_RECONCILING:
		return provisioner.StatusUpdating
	case containerpb.Cluster_STOPPING:
		return provisioner.StatusDeleting
	case containerpb.Cluster_ERROR, containerpb.Cluster_DEGRADED:
		return provisioner.StatusFailed
	default:
		return provisioner.StatusFailed
	}
}

func accessFrom(cfg *containerpb.PrivateClusterConfig) core.Access {
	// PrivateClusterConfig.EnablePrivateEndpoint is deprecated in favour of
	// ControlPlaneEndpointsConfig, but it is what CreateCluster/UpdateCluster
	// above still write, and both remain accepted by the GKE API. Migrating
	// the whole read/write path is tracked separately rather than folded into
	// M2 cluster provisioning.
	if cfg != nil && cfg.GetEnablePrivateEndpoint() { //nolint:staticcheck // see comment above
		return core.AccessPrivate
	}
	return core.AccessPublic
}

func (p *ClusterProvisioner) describeNodePools(ctx context.Context, spec core.ClusterSpec) ([]core.NodePool, error) {
	n := p.names(spec)

	listed, err := p.c.cluster.ListNodePools(ctx, &containerpb.ListNodePoolsRequest{Parent: n.clusterPath()})
	if err != nil {
		return nil, fmt.Errorf("listing node pools for %s: %w", spec.ID, err)
	}

	pools := make([]core.NodePool, 0, len(listed.GetNodePools()))
	for _, np := range listed.GetNodePools() {
		pool := core.NodePool{Name: np.GetName()}
		if cfg := np.GetConfig(); cfg != nil {
			pool.InstanceType = cfg.GetMachineType()
			pool.Labels = cfg.GetLabels()
		}
		if as := np.GetAutoscaling(); as != nil {
			pool.MinSize = as.GetMinNodeCount()
			pool.MaxSize = as.GetMaxNodeCount()
		}
		pool.DesiredSize = np.GetInitialNodeCount()
		pools = append(pools, pool)
	}

	slices.SortFunc(pools, func(a, b core.NodePool) int { return strings.Compare(a.Name, b.Name) })
	return pools, nil
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

	if err := p.ensureNodePools(ctx, spec, &change); err != nil {
		return change, err
	}
	return change, nil
}

func (p *ClusterProvisioner) reconcileAccess(
	ctx context.Context, spec core.ClusterSpec, state provisioner.ClusterState,
) (provisioner.Change, error) {
	if state.Access == spec.Access {
		return provisioner.Change{}, nil
	}

	n := p.names(spec)
	_, err := p.c.cluster.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{
		Name: n.clusterPath(),
		Update: &containerpb.ClusterUpdate{
			DesiredPrivateClusterConfig:           privateClusterConfig(spec),
			DesiredMasterAuthorizedNetworksConfig: authorizedNetworksConfig(spec),
		},
	})
	if err != nil {
		return provisioner.Change{}, fmt.Errorf("updating access mode for %s: %w", spec.ID, err)
	}

	return provisioner.Change{
		Changed: true,
		Details: []string{fmt.Sprintf("access %s -> %s", state.Access, spec.Access)},
	}, nil
}

// ensureNodePools creates missing node pools and resizes drifted ones. It
// never deletes: removing a node pool evicts running workloads, which is a
// decision that belongs to a human rather than to a reconcile loop.
func (p *ClusterProvisioner) ensureNodePools(
	ctx context.Context, spec core.ClusterSpec, change *provisioner.Change,
) error {
	existing, err := p.describeNodePools(ctx, spec)
	if err != nil {
		return err
	}

	n := p.names(spec)
	for _, want := range spec.NodePools {
		current, found := findPool(existing, want.Name)
		if !found {
			if err := p.createNodePool(ctx, spec, want); err != nil {
				return err
			}
			record(change, fmt.Sprintf("create node pool %s", want.Name))
			continue
		}

		if current.MinSize == want.MinSize && current.MaxSize == want.MaxSize &&
			current.DesiredSize == want.DesiredSize {
			continue
		}

		_, err := p.c.cluster.SetNodePoolSize(ctx, &containerpb.SetNodePoolSizeRequest{
			Name:      n.nodePoolPath(want.Name),
			NodeCount: want.DesiredSize,
		})
		if err != nil {
			return fmt.Errorf("resizing node pool %s: %w", want.Name, err)
		}
		record(change, fmt.Sprintf("resize node pool %s to %d/%d/%d",
			want.Name, want.MinSize, want.DesiredSize, want.MaxSize))
	}

	return nil
}

func (p *ClusterProvisioner) createNodePool(ctx context.Context, spec core.ClusterSpec, pool core.NodePool) error {
	n := p.names(spec)

	_, err := p.c.cluster.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{
		Parent: n.clusterPath(),
		NodePool: &containerpb.NodePool{
			Name:             pool.Name,
			InitialNodeCount: pool.DesiredSize,
			Config:           &containerpb.NodeConfig{MachineType: pool.InstanceType, Labels: pool.Labels},
			Autoscaling: &containerpb.NodePoolAutoscaling{
				Enabled:      true,
				MinNodeCount: pool.MinSize,
				MaxNodeCount: pool.MaxSize,
			},
		},
	})
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return nil
		}
		return fmt.Errorf("creating node pool %s: %w", pool.Name, err)
	}
	return nil
}

// Delete tears down the cluster. GKE deletes its node pools along with it, so
// unlike EKS there is no separate node-pool teardown step.
func (p *ClusterProvisioner) Delete(ctx context.Context, spec core.ClusterSpec) error {
	n := p.names(spec)

	if _, err := p.c.cluster.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{Name: n.clusterPath()}); err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}
		return fmt.Errorf("deleting GKE cluster %s: %w", spec.ID, err)
	}
	return nil
}

// validateForGKE covers requirements GKE adds beyond the shared spec rules.
func validateForGKE(spec core.ClusterSpec) error {
	if len(spec.Subnets) == 0 {
		return fmt.Errorf("%w: GKE requires a subnetwork", core.ErrInvalidSpec)
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
