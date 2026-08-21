// Package azure provisions AKS clusters and Workload Identity bindings.
//
// Every Azure service is reached through an interface listing only the calls
// this package makes, the same discipline internal/provisioner/aws and
// internal/provisioner/gcp follow. That keeps the whole provisioner testable
// without credentials, and doubles as the precise permission set an operator
// has to grant.
//
// Azure's control-plane SDK reports long-running operations (cluster create,
// node pool create, NSG rule create) through a poller returned alongside the
// initial response. This package never waits on that poller: like AWS's
// CreateCluster and GKE's CreateCluster, the initial call is what matters —
// it means the request was accepted — and Describe is what a caller polls
// afterwards. The narrow interfaces below reflect that by discarding the
// poller and returning only the error from the request that created it.
package azure

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/msi/armmsi"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// clusterAPI is the AKS surface this package uses.
type clusterAPI interface {
	Get(ctx context.Context, resourceGroup, name string) (*armcontainerservice.ManagedCluster, error)
	CreateOrUpdate(ctx context.Context, resourceGroup, name string, cluster armcontainerservice.ManagedCluster) error
	Delete(ctx context.Context, resourceGroup, name string) error

	GetAgentPool(ctx context.Context, resourceGroup, cluster, pool string) (*armcontainerservice.AgentPool, error)
	ListAgentPools(ctx context.Context, resourceGroup, cluster string) ([]*armcontainerservice.AgentPool, error)
	CreateOrUpdateAgentPool(ctx context.Context, resourceGroup, cluster, pool string, ap armcontainerservice.AgentPool) error

	// ListClusterUserCredentials returns the raw kubeconfig AKS generates for
	// the cluster, the same one `az aks get-credentials` writes to disk. It
	// already carries the cluster's CA data and, for an AAD-enabled cluster,
	// an exec-plugin entry for token refresh — RESTConfig parses it rather
	// than re-deriving the same information from separate calls.
	ListClusterUserCredentials(ctx context.Context, resourceGroup, name string) ([]byte, error)
}

// identityAPI covers the user-assigned managed identity Workload Identity
// binds to, and the federated credential that scopes the binding.
type identityAPI interface {
	GetIdentity(ctx context.Context, resourceGroup, name string) (*armmsi.Identity, error)
	CreateOrUpdateIdentity(ctx context.Context, resourceGroup, name string, id armmsi.Identity) (*armmsi.Identity, error)
	DeleteIdentity(ctx context.Context, resourceGroup, name string) error

	GetFederatedCredential(ctx context.Context, resourceGroup, identityName, name string) (*armmsi.FederatedIdentityCredential, error)
	CreateOrUpdateFederatedCredential(
		ctx context.Context, resourceGroup, identityName, name string, cred armmsi.FederatedIdentityCredential,
	) error
	DeleteFederatedCredential(ctx context.Context, resourceGroup, identityName, name string) error
}

// networkAPI covers the status reporter's egress rule and, for EnsureNetwork,
// the VNet/subnet kubespin creates when none is supplied.
type networkAPI interface {
	ListSecurityGroups(ctx context.Context, resourceGroup string) ([]*armnetwork.SecurityGroup, error)
	GetSecurityRule(ctx context.Context, resourceGroup, nsg, name string) (*armnetwork.SecurityRule, error)
	CreateOrUpdateSecurityRule(ctx context.Context, resourceGroup, nsg, name string, rule armnetwork.SecurityRule) error

	GetVirtualNetwork(ctx context.Context, resourceGroup, name string) (*armnetwork.VirtualNetwork, error)
	CreateOrUpdateVirtualNetwork(ctx context.Context, resourceGroup, name string, vnet armnetwork.VirtualNetwork) error
	GetSubnet(ctx context.Context, resourceGroup, vnet, name string) (*armnetwork.Subnet, error)
	CreateOrUpdateSubnet(ctx context.Context, resourceGroup, vnet, name string, subnet armnetwork.Subnet) error
}

// resourceGroupAPI is the prerequisite every other Azure resource this
// package creates needs: ARM rejects a cluster, identity, or VNet create
// against a resource group that does not exist yet.
type resourceGroupAPI interface {
	GetResourceGroup(ctx context.Context, name string) (bool, error)
	EnsureResourceGroup(ctx context.Context, name, location string) error
	DeleteResourceGroup(ctx context.Context, name string) error
}

// Clients bundles the Azure clients the provisioner uses, scoped to one
// subscription. The subscription is fixed at construction, the way AWS's
// Clients fixes a region and GCP's fixes a project: it is operator
// configuration rather than cluster desired state.
type Clients struct {
	subscription   string
	cluster        clusterAPI
	identity       identityAPI
	network        networkAPI
	resourceGroups resourceGroupAPI

	logger *slog.Logger
}

// Option configures Clients.
type Option func(*Clients)

// WithLogger sets the logger every provisioner built over these Clients logs
// through. Defaults to slog.Default() when not given.
func WithLogger(logger *slog.Logger) Option {
	return func(c *Clients) { c.logger = logger }
}

// NewClients builds real Azure clients for a subscription, authenticating
// with the default credential chain (managed identity in-cluster, az CLI
// locally, environment variables in CI).
func NewClients(subscription string, opts ...Option) (*Clients, error) {
	if subscription == "" {
		return nil, fmt.Errorf("azure: subscription is required")
	}

	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("building Azure credential: %w", err)
	}

	clusters, err := armcontainerservice.NewManagedClustersClient(subscription, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("building AKS client: %w", err)
	}
	agentPools, err := armcontainerservice.NewAgentPoolsClient(subscription, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("building AKS agent pools client: %w", err)
	}
	identities, err := armmsi.NewUserAssignedIdentitiesClient(subscription, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("building managed identities client: %w", err)
	}
	federated, err := armmsi.NewFederatedIdentityCredentialsClient(subscription, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("building federated credentials client: %w", err)
	}
	securityGroups, err := armnetwork.NewSecurityGroupsClient(subscription, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("building NSG client: %w", err)
	}
	securityRules, err := armnetwork.NewSecurityRulesClient(subscription, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("building NSG rules client: %w", err)
	}
	vnets, err := armnetwork.NewVirtualNetworksClient(subscription, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("building virtual networks client: %w", err)
	}
	subnets, err := armnetwork.NewSubnetsClient(subscription, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("building subnets client: %w", err)
	}
	resourceGroups, err := armresources.NewResourceGroupsClient(subscription, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("building resource groups client: %w", err)
	}

	c := &Clients{
		subscription: subscription,
		cluster:      realCluster{clusters: clusters, agentPools: agentPools},
		identity:     realIdentity{identities: identities, federated: federated},
		network: realNetwork{
			groups: securityGroups, rules: securityRules, vnets: vnets, subnets: subnets,
		},
		resourceGroups: realResourceGroups{groups: resourceGroups},
		logger:         slog.Default(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// realCluster adapts the poller-returning AKS clients to clusterAPI.
type realCluster struct {
	clusters   *armcontainerservice.ManagedClustersClient
	agentPools *armcontainerservice.AgentPoolsClient
}

func (r realCluster) Get(ctx context.Context, rg, name string) (*armcontainerservice.ManagedCluster, error) {
	resp, err := r.clusters.Get(ctx, rg, name, nil)
	if err != nil {
		return nil, fmt.Errorf("aks: get cluster %s/%s: %w", rg, name, err)
	}
	return &resp.ManagedCluster, nil
}

func (r realCluster) CreateOrUpdate(ctx context.Context, rg, name string, cluster armcontainerservice.ManagedCluster) error {
	if _, err := r.clusters.BeginCreateOrUpdate(ctx, rg, name, cluster, nil); err != nil {
		return fmt.Errorf("aks: create or update cluster %s/%s: %w", rg, name, err)
	}
	return nil
}

func (r realCluster) Delete(ctx context.Context, rg, name string) error {
	if _, err := r.clusters.BeginDelete(ctx, rg, name, nil); err != nil {
		return fmt.Errorf("aks: delete cluster %s/%s: %w", rg, name, err)
	}
	return nil
}

func (r realCluster) GetAgentPool(ctx context.Context, rg, cluster, pool string) (*armcontainerservice.AgentPool, error) {
	resp, err := r.agentPools.Get(ctx, rg, cluster, pool, nil)
	if err != nil {
		return nil, fmt.Errorf("aks: get agent pool %s/%s/%s: %w", rg, cluster, pool, err)
	}
	return &resp.AgentPool, nil
}

func (r realCluster) ListAgentPools(ctx context.Context, rg, cluster string) ([]*armcontainerservice.AgentPool, error) {
	pager := r.agentPools.NewListPager(rg, cluster, nil)
	var out []*armcontainerservice.AgentPool
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("aks: list agent pools %s/%s: %w", rg, cluster, err)
		}
		out = append(out, page.Value...)
	}
	return out, nil
}

func (r realCluster) CreateOrUpdateAgentPool(
	ctx context.Context, rg, cluster, pool string, ap armcontainerservice.AgentPool,
) error {
	if _, err := r.agentPools.BeginCreateOrUpdate(ctx, rg, cluster, pool, ap, nil); err != nil {
		return fmt.Errorf("aks: create or update agent pool %s/%s/%s: %w", rg, cluster, pool, err)
	}
	return nil
}

func (r realCluster) ListClusterUserCredentials(ctx context.Context, rg, name string) ([]byte, error) {
	resp, err := r.clusters.ListClusterUserCredentials(ctx, rg, name, nil)
	if err != nil {
		return nil, fmt.Errorf("aks: list user credentials for %s/%s: %w", rg, name, err)
	}
	for _, kc := range resp.Kubeconfigs {
		if kc != nil && len(kc.Value) > 0 {
			return kc.Value, nil
		}
	}
	return nil, fmt.Errorf("aks: no kubeconfig returned for %s/%s", rg, name)
}

// realIdentity adapts the synchronous MSI clients to identityAPI. Unlike AKS,
// UserAssignedIdentitiesClient and FederatedIdentityCredentialsClient are not
// long-running: creating a managed identity or a federated credential
// completes within the request.
type realIdentity struct {
	identities *armmsi.UserAssignedIdentitiesClient
	federated  *armmsi.FederatedIdentityCredentialsClient
}

func (r realIdentity) GetIdentity(ctx context.Context, rg, name string) (*armmsi.Identity, error) {
	resp, err := r.identities.Get(ctx, rg, name, nil)
	if err != nil {
		return nil, fmt.Errorf("msi: get identity %s/%s: %w", rg, name, err)
	}
	return &resp.Identity, nil
}

func (r realIdentity) CreateOrUpdateIdentity(ctx context.Context, rg, name string, id armmsi.Identity) (*armmsi.Identity, error) {
	resp, err := r.identities.CreateOrUpdate(ctx, rg, name, id, nil)
	if err != nil {
		return nil, fmt.Errorf("msi: create or update identity %s/%s: %w", rg, name, err)
	}
	return &resp.Identity, nil
}

func (r realIdentity) DeleteIdentity(ctx context.Context, rg, name string) error {
	if _, err := r.identities.Delete(ctx, rg, name, nil); err != nil {
		return fmt.Errorf("msi: delete identity %s/%s: %w", rg, name, err)
	}
	return nil
}

func (r realIdentity) GetFederatedCredential(
	ctx context.Context, rg, identityName, name string,
) (*armmsi.FederatedIdentityCredential, error) {
	resp, err := r.federated.Get(ctx, rg, identityName, name, nil)
	if err != nil {
		return nil, fmt.Errorf("msi: get federated credential %s/%s/%s: %w", rg, identityName, name, err)
	}
	return &resp.FederatedIdentityCredential, nil
}

func (r realIdentity) CreateOrUpdateFederatedCredential(
	ctx context.Context, rg, identityName, name string, cred armmsi.FederatedIdentityCredential,
) error {
	if _, err := r.federated.CreateOrUpdate(ctx, rg, identityName, name, cred, nil); err != nil {
		return fmt.Errorf("msi: create or update federated credential %s/%s/%s: %w", rg, identityName, name, err)
	}
	return nil
}

func (r realIdentity) DeleteFederatedCredential(ctx context.Context, rg, identityName, name string) error {
	if _, err := r.federated.Delete(ctx, rg, identityName, name, nil); err != nil {
		return fmt.Errorf("msi: delete federated credential %s/%s/%s: %w", rg, identityName, name, err)
	}
	return nil
}

// realNetwork adapts the NSG, VNet, and subnet clients to networkAPI.
type realNetwork struct {
	groups  *armnetwork.SecurityGroupsClient
	rules   *armnetwork.SecurityRulesClient
	vnets   *armnetwork.VirtualNetworksClient
	subnets *armnetwork.SubnetsClient
}

func (r realNetwork) ListSecurityGroups(ctx context.Context, rg string) ([]*armnetwork.SecurityGroup, error) {
	pager := r.groups.NewListPager(rg, nil)
	var out []*armnetwork.SecurityGroup
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("network: list security groups in %s: %w", rg, err)
		}
		out = append(out, page.Value...)
	}
	return out, nil
}

func (r realNetwork) GetSecurityRule(ctx context.Context, rg, nsg, name string) (*armnetwork.SecurityRule, error) {
	resp, err := r.rules.Get(ctx, rg, nsg, name, nil)
	if err != nil {
		return nil, fmt.Errorf("network: get security rule %s/%s/%s: %w", rg, nsg, name, err)
	}
	return &resp.SecurityRule, nil
}

func (r realNetwork) CreateOrUpdateSecurityRule(ctx context.Context, rg, nsg, name string, rule armnetwork.SecurityRule) error {
	if _, err := r.rules.BeginCreateOrUpdate(ctx, rg, nsg, name, rule, nil); err != nil {
		return fmt.Errorf("network: create or update security rule %s/%s/%s: %w", rg, nsg, name, err)
	}
	return nil
}

func (r realNetwork) GetVirtualNetwork(ctx context.Context, rg, name string) (*armnetwork.VirtualNetwork, error) {
	resp, err := r.vnets.Get(ctx, rg, name, nil)
	if err != nil {
		return nil, fmt.Errorf("network: get virtual network %s/%s: %w", rg, name, err)
	}
	return &resp.VirtualNetwork, nil
}

func (r realNetwork) CreateOrUpdateVirtualNetwork(ctx context.Context, rg, name string, vnet armnetwork.VirtualNetwork) error {
	if _, err := r.vnets.BeginCreateOrUpdate(ctx, rg, name, vnet, nil); err != nil {
		return fmt.Errorf("network: create or update virtual network %s/%s: %w", rg, name, err)
	}
	return nil
}

func (r realNetwork) GetSubnet(ctx context.Context, rg, vnet, name string) (*armnetwork.Subnet, error) {
	resp, err := r.subnets.Get(ctx, rg, vnet, name, nil)
	if err != nil {
		return nil, fmt.Errorf("network: get subnet %s/%s/%s: %w", rg, vnet, name, err)
	}
	return &resp.Subnet, nil
}

func (r realNetwork) CreateOrUpdateSubnet(ctx context.Context, rg, vnet, name string, subnet armnetwork.Subnet) error {
	if _, err := r.subnets.BeginCreateOrUpdate(ctx, rg, vnet, name, subnet, nil); err != nil {
		return fmt.Errorf("network: create or update subnet %s/%s/%s: %w", rg, vnet, name, err)
	}
	return nil
}

// realResourceGroups adapts the resource groups client to resourceGroupAPI.
// Unlike the network and cluster clients, CreateOrUpdate here is synchronous —
// ARM does not treat resource group creation as a long-running operation.
type realResourceGroups struct {
	groups *armresources.ResourceGroupsClient
}

func (r realResourceGroups) GetResourceGroup(ctx context.Context, name string) (bool, error) {
	resp, err := r.groups.CheckExistence(ctx, name, nil)
	if err != nil {
		return false, fmt.Errorf("resources: checking resource group %s: %w", name, err)
	}
	return resp.Success, nil
}

func (r realResourceGroups) EnsureResourceGroup(ctx context.Context, name, location string) error {
	_, err := r.groups.CreateOrUpdate(ctx, name, armresources.ResourceGroup{
		Location: ptr(location),
	}, nil)
	if err != nil {
		return fmt.Errorf("resources: create or update resource group %s: %w", name, err)
	}
	return nil
}

// DeleteResourceGroup deletes the group and everything in it — the cluster,
// and the VNet/subnet if kubespin created them — and waits for the deletion
// to finish, unlike Delete on the cluster/node-pool clients, which are
// deliberately fire-and-forget for the orchestrator's own WaitUntilGone to
// poll. Nothing polls a network's teardown separately, so this is the one
// place that has to wait for itself.
func (r realResourceGroups) DeleteResourceGroup(ctx context.Context, name string) error {
	poller, err := r.groups.BeginDelete(ctx, name, nil)
	if err != nil {
		return fmt.Errorf("resources: delete resource group %s: %w", name, err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("resources: delete resource group %s: %w", name, err)
	}
	return nil
}

// code extracts the HTTP status code from an azcore response error, or 0 if
// err is not one.
func code(err error) int {
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		return respErr.StatusCode
	}
	return 0
}

// names derives every Azure resource name from the cluster ID, so a
// cluster's resources are identifiable and a second cluster cannot collide
// with them.
type names struct {
	spec core.ClusterSpec
}

func (n names) resourceGroup() string { return "kubespin-" + n.spec.ID.String() }
func (n names) cluster() string       { return n.spec.ID.String() }
func (n names) identity(comp string) string {
	return "kubespin-" + n.spec.ID.String() + "-" + comp
}
func (n names) federatedCredential(comp string) string { return comp }
func (n names) securityRule() string                   { return "kubespin-" + n.spec.ID.String() + "-egress" }
func (n names) vnet() string                           { return "kubespin-" + n.spec.ID.String() + "-vnet" }
func (n names) subnet() string                         { return "kubespin-" + n.spec.ID.String() + "-subnet" }

func tags(spec core.ClusterSpec) map[string]*string {
	managedBy, cluster, size := "kubespin", spec.ID.String(), spec.Size.String()
	return map[string]*string{
		"ManagedBy":        &managedBy,
		"kubespin-cluster": &cluster,
		"kubespin-size":    &size,
	}
}
