package azure

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v6"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

// serviceCIDR/dnsServiceIP are the Kubernetes service network AKS is given.
// AKS otherwise defaults the service CIDR to 10.0.0.0/16, which collides with
// the 10.0.0.0/16 VNet (and its 10.0.1.0/24 subnet) EnsureNetwork creates by
// default, so an explicit, disjoint range must always be supplied.
const (
	serviceCIDR  = "172.16.0.0/16"
	dnsServiceIP = "172.16.0.10"
)

// ClusterProvisioner creates and reconciles AKS clusters.
type ClusterProvisioner struct {
	c *Clients
}

// NewClusterProvisioner builds an AKS provisioner over the given clients.
func NewClusterProvisioner(c *Clients) *ClusterProvisioner { return &ClusterProvisioner{c: c} }

// Provider identifies this implementation's cloud.
func (p *ClusterProvisioner) Provider() core.Provider { return core.ProviderAzure }

// Create requests a cluster and its node pools.
//
// It is idempotent at every step: an existing cluster or node pool is left
// alone rather than treated as an error, so a resumed run passes straight
// through to whatever is still missing.
func (p *ClusterProvisioner) Create(ctx context.Context, spec core.ClusterSpec) error {
	if err := validateForAKS(spec); err != nil {
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
	n := names{spec}

	location := spec.Region
	subnetID := ""
	if len(spec.Subnets) > 0 {
		subnetID = spec.Subnets[0]
	}
	agentPools := []*armcontainerservice.ManagedClusterAgentPoolProfile{}
	if len(spec.NodePools) > 0 {
		agentPools = append(agentPools, agentPoolProfile(spec.NodePools[0], subnetID))
	}

	cluster := armcontainerservice.ManagedCluster{
		Location: &location,
		Identity: &armcontainerservice.ManagedClusterIdentity{
			Type: ptr(armcontainerservice.ResourceIdentityTypeSystemAssigned),
		},
		Tags: tags(spec),
		Properties: &armcontainerservice.ManagedClusterProperties{
			DNSPrefix:         ptr(n.cluster()),
			AgentPoolProfiles: agentPools,
			APIServerAccessProfile: &armcontainerservice.ManagedClusterAPIServerAccessProfile{
				EnablePrivateCluster: ptr(spec.Access == core.AccessPrivate),
				AuthorizedIPRanges:   ptrSlice(spec.AuthorizedCIDRs),
			},
			OidcIssuerProfile: &armcontainerservice.ManagedClusterOIDCIssuerProfile{
				Enabled: ptr(true),
			},
			SecurityProfile: &armcontainerservice.ManagedClusterSecurityProfile{
				WorkloadIdentity: &armcontainerservice.ManagedClusterSecurityProfileWorkloadIdentity{
					Enabled: ptr(true),
				},
			},
			NetworkProfile: &armcontainerservice.NetworkProfile{
				NetworkPlugin: ptr(armcontainerservice.NetworkPluginAzure),
				ServiceCidr:   ptr(serviceCIDR),
				DNSServiceIP:  ptr(dnsServiceIP),
			},
		},
	}
	if spec.KubernetesVersion != "" {
		cluster.Properties.KubernetesVersion = ptr(spec.KubernetesVersion)
	}

	if err := p.c.cluster.CreateOrUpdate(ctx, n.resourceGroup(), n.cluster(), cluster); err != nil {
		return fmt.Errorf("creating AKS cluster %s: %w", spec.ID, err)
	}
	return nil
}

func agentPoolProfile(pool core.NodePool, subnetID string) *armcontainerservice.ManagedClusterAgentPoolProfile {
	name := pool.Name
	profile := &armcontainerservice.ManagedClusterAgentPoolProfile{
		Name:              &name,
		VMSize:            ptr(pool.InstanceType),
		Count:             ptr(pool.DesiredSize),
		MinCount:          ptr(pool.MinSize),
		MaxCount:          ptr(pool.MaxSize),
		EnableAutoScaling: ptr(true),
		NodeLabels:        ptrMap(pool.Labels),
		Mode:              ptr(armcontainerservice.AgentPoolModeSystem),
	}
	if subnetID != "" {
		profile.VnetSubnetID = ptr(subnetID)
	}
	return profile
}

// Describe reports the cluster's current state.
func (p *ClusterProvisioner) Describe(ctx context.Context, spec core.ClusterSpec) (provisioner.ClusterState, error) {
	n := names{spec}

	cluster, err := p.c.cluster.Get(ctx, n.resourceGroup(), n.cluster())
	if err != nil {
		if code(err) == 404 {
			// Absent is a normal answer while polling, not an error.
			return provisioner.ClusterState{Status: provisioner.StatusAbsent}, nil
		}
		return provisioner.ClusterState{}, fmt.Errorf("describing AKS cluster %s: %w", spec.ID, err)
	}
	if cluster.Properties == nil {
		return provisioner.ClusterState{Status: provisioner.StatusCreating}, nil
	}

	props := cluster.Properties
	state := provisioner.ClusterState{
		Status:  normaliseStatus(deref(props.ProvisioningState)),
		Version: deref(props.CurrentKubernetesVersion),
		Access:  accessFrom(props.APIServerAccessProfile),
	}
	if props.Fqdn != nil {
		state.Endpoint = deref(props.Fqdn)
	} else if props.PrivateFQDN != nil {
		state.Endpoint = deref(props.PrivateFQDN)
	}
	if props.OidcIssuerProfile != nil {
		state.OIDCIssuer = deref(props.OidcIssuerProfile.IssuerURL)
	}
	if props.NodeResourceGroup != nil {
		// The node resource group is where AKS places the cluster's NSG, so it
		// is the network scope egress rules are provisioned against.
		state.NetworkID = deref(props.NodeResourceGroup)
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

func normaliseStatus(provisioningState string) provisioner.Status {
	switch provisioningState {
	case "Succeeded":
		return provisioner.StatusActive
	case "Creating":
		return provisioner.StatusCreating
	case "Updating", "Upgrading", "Scaling":
		return provisioner.StatusUpdating
	case "Deleting":
		return provisioner.StatusDeleting
	case "Failed", "Canceled":
		return provisioner.StatusFailed
	default:
		return provisioner.StatusCreating
	}
}

func accessFrom(profile *armcontainerservice.ManagedClusterAPIServerAccessProfile) core.Access {
	if profile != nil && profile.EnablePrivateCluster != nil && *profile.EnablePrivateCluster {
		return core.AccessPrivate
	}
	return core.AccessPublic
}

func (p *ClusterProvisioner) describeNodePools(ctx context.Context, spec core.ClusterSpec) ([]core.NodePool, error) {
	n := names{spec}

	listed, err := p.c.cluster.ListAgentPools(ctx, n.resourceGroup(), n.cluster())
	if err != nil {
		return nil, fmt.Errorf("listing node pools for %s: %w", spec.ID, err)
	}

	pools := make([]core.NodePool, 0, len(listed))
	for _, ap := range listed {
		if ap.Properties == nil {
			continue
		}
		pool := core.NodePool{
			Name:         deref(ap.Name),
			InstanceType: deref(ap.Properties.VMSize),
			MinSize:      derefInt32(ap.Properties.MinCount),
			MaxSize:      derefInt32(ap.Properties.MaxCount),
			DesiredSize:  derefInt32(ap.Properties.Count),
			Labels:       derefMap(ap.Properties.NodeLabels),
		}
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

	n := names{spec}
	cluster, err := p.c.cluster.Get(ctx, n.resourceGroup(), n.cluster())
	if err != nil {
		return provisioner.Change{}, fmt.Errorf("reading %s before updating access: %w", spec.ID, err)
	}
	cluster.Properties.APIServerAccessProfile = &armcontainerservice.ManagedClusterAPIServerAccessProfile{
		EnablePrivateCluster: ptr(spec.Access == core.AccessPrivate),
		AuthorizedIPRanges:   ptrSlice(spec.AuthorizedCIDRs),
	}

	if err := p.c.cluster.CreateOrUpdate(ctx, n.resourceGroup(), n.cluster(), *cluster); err != nil {
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

	n := names{spec}
	subnetID := ""
	if len(spec.Subnets) > 0 {
		subnetID = spec.Subnets[0]
	}
	for _, want := range spec.NodePools {
		current, found := findPool(existing, want.Name)
		if !found {
			if err := p.c.cluster.CreateOrUpdateAgentPool(
				ctx, n.resourceGroup(), n.cluster(), want.Name, *agentPoolProfileAsPool(want, subnetID),
			); err != nil {
				return fmt.Errorf("creating node pool %s: %w", want.Name, err)
			}
			record(change, fmt.Sprintf("create node pool %s", want.Name))
			continue
		}

		if current.InstanceType != want.InstanceType {
			return fmt.Errorf(
				"node pool %s: instance type is %s, spec wants %s: AKS does not allow changing "+
					"vmSize on an existing agent pool; create a differently-named node pool instead",
				want.Name, current.InstanceType, want.InstanceType,
			)
		}

		if current.MinSize == want.MinSize && current.MaxSize == want.MaxSize &&
			current.DesiredSize == want.DesiredSize {
			continue
		}

		if err := p.c.cluster.CreateOrUpdateAgentPool(
			ctx, n.resourceGroup(), n.cluster(), want.Name, *agentPoolProfileAsPool(want, subnetID),
		); err != nil {
			return fmt.Errorf("resizing node pool %s: %w", want.Name, err)
		}
		record(change, fmt.Sprintf("resize node pool %s to %d/%d/%d",
			want.Name, want.MinSize, want.DesiredSize, want.MaxSize))
	}

	return nil
}

func agentPoolProfileAsPool(pool core.NodePool, subnetID string) *armcontainerservice.AgentPool {
	profile := agentPoolProfile(pool, subnetID)
	return &armcontainerservice.AgentPool{
		Name: profile.Name,
		Properties: &armcontainerservice.ManagedClusterAgentPoolProfileProperties{
			VMSize:            profile.VMSize,
			Count:             profile.Count,
			MinCount:          profile.MinCount,
			MaxCount:          profile.MaxCount,
			EnableAutoScaling: profile.EnableAutoScaling,
			NodeLabels:        profile.NodeLabels,
			Mode:              profile.Mode,
			VnetSubnetID:      profile.VnetSubnetID,
		},
	}
}

// Delete tears down the cluster. AKS deletes its node pools along with it, so
// unlike EKS there is no separate node-pool teardown step.
//
// Deletion is asynchronous: BeginDelete starts the long-running operation and
// this returns once Azure has accepted it, leaving the caller to poll Describe
// (provisioner.WaitUntilGone) until the cluster is really gone. A cluster
// already tearing down is convergence rather than an error — Azure answers a
// second delete against a Deleting cluster with 409, and a retried teardown
// has to resume, not fail.
func (p *ClusterProvisioner) Delete(ctx context.Context, spec core.ClusterSpec) error {
	if done, err := p.alreadyGoing(ctx, spec); err != nil || done {
		return err
	}

	n := names{spec}
	if err := p.c.cluster.Delete(ctx, n.resourceGroup(), n.cluster()); err != nil {
		if code(err) == 404 {
			return nil
		}
		// Lost a race with another teardown between the check above and here.
		if code(err) == 409 {
			if done, derr := p.alreadyGoing(ctx, spec); derr == nil && done {
				return nil
			}
		}
		return fmt.Errorf("deleting AKS cluster %s: %w", spec.ID, err)
	}
	return nil
}

// alreadyGoing reports whether the cluster is gone or on its way out, in which
// case there is nothing left for Delete to request.
func (p *ClusterProvisioner) alreadyGoing(ctx context.Context, spec core.ClusterSpec) (bool, error) {
	state, err := p.Describe(ctx, spec)
	if err != nil {
		return false, err
	}
	return state.Status == provisioner.StatusAbsent || state.Status == provisioner.StatusDeleting, nil
}

// validateForAKS covers requirements AKS adds beyond the shared spec rules.
func validateForAKS(spec core.ClusterSpec) error {
	if len(spec.Subnets) == 0 {
		return fmt.Errorf("%w: AKS requires a subnet", core.ErrInvalidSpec)
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

// --- pointer helpers ---
//
// The Azure SDK represents every optional field as a pointer, so these
// convert between kubespin's plain core types and the SDK's *T convention.

func ptr[T any](v T) *T { return &v }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt32(v *int32) int32 {
	if v == nil {
		return 0
	}
	return *v
}

func ptrSlice(ss []string) []*string {
	if len(ss) == 0 {
		return nil
	}
	out := make([]*string, len(ss))
	for i, s := range ss {
		out[i] = &s
	}
	return out
}

func ptrMap(m map[string]string) map[string]*string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]*string, len(m))
	for k, v := range m {
		out[k] = &v
	}
	return out
}

func derefMap(m map[string]*string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = deref(v)
	}
	return out
}
