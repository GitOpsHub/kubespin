package azure

import (
	"context"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/msi/armmsi"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// fakeAzure stands in for the AKS, MSI, and Network clients, recording every
// call by name so tests can assert which calls were made — which is how
// "reconcile changed nothing" is held to making no mutating calls at all.
type fakeAzure struct {
	calls []string

	cluster        *armcontainerservice.ManagedCluster
	agentPools     map[string]*armcontainerservice.AgentPool
	identities     map[string]*armmsi.Identity
	federated      map[string]*armmsi.FederatedIdentityCredential // "identity/credName" -> cred
	nsgs           map[string][]*armnetwork.SecurityGroup         // resourceGroup -> NSGs
	rules          map[string]*armnetwork.SecurityRule            // "nsg/ruleName" -> rule
	resourceGroups map[string]string                              // name -> location
	vnets          map[string]*armnetwork.VirtualNetwork          // "rg/vnet" -> vnet
	subnets        map[string]*armnetwork.Subnet                  // "rg/vnet/subnet" -> subnet
}

func newFakeAzure() *fakeAzure {
	return &fakeAzure{
		agentPools:     map[string]*armcontainerservice.AgentPool{},
		identities:     map[string]*armmsi.Identity{},
		federated:      map[string]*armmsi.FederatedIdentityCredential{},
		nsgs:           map[string][]*armnetwork.SecurityGroup{},
		rules:          map[string]*armnetwork.SecurityRule{},
		resourceGroups: map[string]string{},
		vnets:          map[string]*armnetwork.VirtualNetwork{},
		subnets:        map[string]*armnetwork.Subnet{},
	}
}

func (f *fakeAzure) record(name string) { f.calls = append(f.calls, name) }

var mutatingCalls = []string{
	"CreateOrUpdateCluster", "DeleteCluster", "CreateOrUpdateAgentPool",
	"CreateOrUpdateIdentity", "DeleteIdentity",
	"CreateOrUpdateFederatedCredential", "DeleteFederatedCredential",
	"CreateOrUpdateSecurityRule",
	"EnsureResourceGroup", "CreateOrUpdateVirtualNetwork", "CreateOrUpdateSubnet",
}

func (f *fakeAzure) assertNoMutations(t *testing.T) {
	t.Helper()
	for _, call := range f.calls {
		for _, mutator := range mutatingCalls {
			if call == mutator {
				t.Errorf("expected no cloud changes, but %s was called", call)
			}
		}
	}
}

func (f *fakeAzure) clients() *Clients {
	return &Clients{
		subscription: testSubscription, cluster: f, identity: f, network: f, resourceGroups: f,
	}
}

func notFound() error { return &azcore.ResponseError{StatusCode: 404} }

// --- AKS ---

func (f *fakeAzure) Get(_ context.Context, _, name string) (*armcontainerservice.ManagedCluster, error) {
	f.record("GetCluster")
	if f.cluster == nil || deref(f.cluster.Name) != name {
		return nil, notFound()
	}
	return f.cluster, nil
}

func (f *fakeAzure) CreateOrUpdate(_ context.Context, _, name string, cluster armcontainerservice.ManagedCluster) error {
	f.record("CreateOrUpdateCluster")
	cluster.Name = ptr(name)
	if cluster.Properties == nil {
		cluster.Properties = &armcontainerservice.ManagedClusterProperties{}
	}
	if cluster.Properties.ProvisioningState == nil {
		cluster.Properties.ProvisioningState = ptr("Creating")
	}
	if cluster.Properties.NodeResourceGroup == nil {
		cluster.Properties.NodeResourceGroup = ptr("MC_" + name)
	}
	f.cluster = &cluster
	for _, ap := range cluster.Properties.AgentPoolProfiles {
		f.agentPools[deref(ap.Name)] = &armcontainerservice.AgentPool{
			Name: ap.Name,
			Properties: &armcontainerservice.ManagedClusterAgentPoolProfileProperties{
				VMSize: ap.VMSize, Count: ap.Count, MinCount: ap.MinCount, MaxCount: ap.MaxCount,
				NodeLabels: ap.NodeLabels, VnetSubnetID: ap.VnetSubnetID,
			},
		}
	}
	return nil
}

func (f *fakeAzure) Delete(_ context.Context, _, _ string) error {
	f.record("DeleteCluster")
	if f.cluster == nil {
		return notFound()
	}
	f.cluster = nil
	f.agentPools = map[string]*armcontainerservice.AgentPool{}
	return nil
}

func (f *fakeAzure) GetAgentPool(_ context.Context, _, _, pool string) (*armcontainerservice.AgentPool, error) {
	f.record("GetAgentPool")
	ap, ok := f.agentPools[pool]
	if !ok {
		return nil, notFound()
	}
	return ap, nil
}

func (f *fakeAzure) ListAgentPools(context.Context, string, string) ([]*armcontainerservice.AgentPool, error) {
	f.record("ListAgentPools")
	out := make([]*armcontainerservice.AgentPool, 0, len(f.agentPools))
	for _, ap := range f.agentPools {
		out = append(out, ap)
	}
	return out, nil
}

func (f *fakeAzure) CreateOrUpdateAgentPool(_ context.Context, _, _, pool string, ap armcontainerservice.AgentPool) error {
	f.record("CreateOrUpdateAgentPool")
	ap.Name = ptr(pool)
	f.agentPools[pool] = &ap
	return nil
}

// --- MSI ---

func (f *fakeAzure) GetIdentity(_ context.Context, _, name string) (*armmsi.Identity, error) {
	f.record("GetIdentity")
	id, ok := f.identities[name]
	if !ok {
		return nil, notFound()
	}
	return id, nil
}

func (f *fakeAzure) CreateOrUpdateIdentity(_ context.Context, _, name string, id armmsi.Identity) (*armmsi.Identity, error) {
	f.record("CreateOrUpdateIdentity")
	id.Name = ptr(name)
	if id.Properties == nil {
		id.Properties = &armmsi.UserAssignedIdentityProperties{}
	}
	if id.Properties.ClientID == nil {
		id.Properties.ClientID = ptr("client-" + name)
	}
	f.identities[name] = &id
	return &id, nil
}

func (f *fakeAzure) DeleteIdentity(_ context.Context, _, name string) error {
	f.record("DeleteIdentity")
	if _, ok := f.identities[name]; !ok {
		return notFound()
	}
	delete(f.identities, name)
	return nil
}

func (f *fakeAzure) GetFederatedCredential(_ context.Context, _, identityName, name string) (*armmsi.FederatedIdentityCredential, error) {
	f.record("GetFederatedCredential")
	cred, ok := f.federated[identityName+"/"+name]
	if !ok {
		return nil, notFound()
	}
	return cred, nil
}

func (f *fakeAzure) CreateOrUpdateFederatedCredential(
	_ context.Context, _, identityName, name string, cred armmsi.FederatedIdentityCredential,
) error {
	f.record("CreateOrUpdateFederatedCredential")
	cred.Name = ptr(name)
	f.federated[identityName+"/"+name] = &cred
	return nil
}

func (f *fakeAzure) DeleteFederatedCredential(_ context.Context, _, identityName, name string) error {
	f.record("DeleteFederatedCredential")
	key := identityName + "/" + name
	if _, ok := f.federated[key]; !ok {
		return notFound()
	}
	delete(f.federated, key)
	return nil
}

// --- Network ---

func (f *fakeAzure) ListSecurityGroups(_ context.Context, rg string) ([]*armnetwork.SecurityGroup, error) {
	f.record("ListSecurityGroups")
	return f.nsgs[rg], nil
}

func (f *fakeAzure) GetSecurityRule(_ context.Context, _, nsg, name string) (*armnetwork.SecurityRule, error) {
	f.record("GetSecurityRule")
	rule, ok := f.rules[nsg+"/"+name]
	if !ok {
		return nil, notFound()
	}
	return rule, nil
}

func (f *fakeAzure) CreateOrUpdateSecurityRule(_ context.Context, _, nsg, name string, rule armnetwork.SecurityRule) error {
	f.record("CreateOrUpdateSecurityRule")
	rule.Name = ptr(name)
	f.rules[nsg+"/"+name] = &rule
	return nil
}

func (f *fakeAzure) GetVirtualNetwork(_ context.Context, rg, name string) (*armnetwork.VirtualNetwork, error) {
	f.record("GetVirtualNetwork")
	vnet, ok := f.vnets[rg+"/"+name]
	if !ok {
		return nil, notFound()
	}
	return vnet, nil
}

func (f *fakeAzure) CreateOrUpdateVirtualNetwork(_ context.Context, rg, name string, vnet armnetwork.VirtualNetwork) error {
	f.record("CreateOrUpdateVirtualNetwork")
	vnet.Name = ptr(name)
	vnet.ID = ptr("/subscriptions/" + testSubscription + "/resourceGroups/" + rg +
		"/providers/Microsoft.Network/virtualNetworks/" + name)
	f.vnets[rg+"/"+name] = &vnet
	return nil
}

func (f *fakeAzure) GetSubnet(_ context.Context, rg, vnet, name string) (*armnetwork.Subnet, error) {
	f.record("GetSubnet")
	subnet, ok := f.subnets[rg+"/"+vnet+"/"+name]
	if !ok {
		return nil, notFound()
	}
	return subnet, nil
}

func (f *fakeAzure) CreateOrUpdateSubnet(_ context.Context, rg, vnet, name string, subnet armnetwork.Subnet) error {
	f.record("CreateOrUpdateSubnet")
	subnet.Name = ptr(name)
	subnet.ID = ptr("/subscriptions/" + testSubscription + "/resourceGroups/" + rg +
		"/providers/Microsoft.Network/virtualNetworks/" + vnet + "/subnets/" + name)
	f.subnets[rg+"/"+vnet+"/"+name] = &subnet
	return nil
}

// --- Resource Groups ---

func (f *fakeAzure) GetResourceGroup(_ context.Context, name string) (bool, error) {
	f.record("GetResourceGroup")
	_, ok := f.resourceGroups[name]
	return ok, nil
}

func (f *fakeAzure) EnsureResourceGroup(_ context.Context, name, location string) error {
	f.record("EnsureResourceGroup")
	f.resourceGroups[name] = location
	return nil
}

// --- helpers ---

const testSubscription = "11111111-1111-1111-1111-111111111111"

func testSpec() core.ClusterSpec {
	return core.ClusterSpec{
		ID:       "team-payments-prod",
		Provider: core.ProviderAzure,
		Region:   "eastus",
		Access:   core.AccessPrivate,
		Subnets:  []string{"/subscriptions/x/resourceGroups/net/providers/Microsoft.Network/virtualNetworks/vnet/subnets/default"},
		NodePools: []core.NodePool{{
			Name: "default", InstanceType: "Standard_D4s_v5", MinSize: 1, MaxSize: 5, DesiredSize: 3,
		}},
		Profile: core.ProfileRef{Name: "tier-small", Version: "1.0.0"},
	}
}

const testIssuer = "https://eastus.oic.prod-aks.azure.com/tenant-id/cluster-id/"

// activeCluster puts the fake into the state a successfully created cluster
// leaves behind.
func (f *fakeAzure) activeCluster(spec core.ClusterSpec) {
	n := names{spec}
	nrg := "MC_" + n.resourceGroup()
	f.cluster = &armcontainerservice.ManagedCluster{
		Name: ptr(n.cluster()),
		Properties: &armcontainerservice.ManagedClusterProperties{
			ProvisioningState: ptr("Succeeded"),
			Fqdn:              ptr("team-payments-prod.eastus.azmk8s.io"),
			NodeResourceGroup: ptr(nrg),
			OidcIssuerProfile: &armcontainerservice.ManagedClusterOIDCIssuerProfile{
				Enabled: ptr(true), IssuerURL: ptr(testIssuer),
			},
			APIServerAccessProfile: &armcontainerservice.ManagedClusterAPIServerAccessProfile{
				EnablePrivateCluster: ptr(spec.Access == core.AccessPrivate),
			},
		},
	}
	f.nsgs[nrg] = []*armnetwork.SecurityGroup{{Name: ptr("aks-agentpool-nsg")}}
}

// withNodePool registers an agent pool matching the given pool.
func (f *fakeAzure) withNodePool(pool core.NodePool) {
	f.agentPools[pool.Name] = &armcontainerservice.AgentPool{
		Name: ptr(pool.Name),
		Properties: &armcontainerservice.ManagedClusterAgentPoolProfileProperties{
			VMSize:   ptr(pool.InstanceType),
			Count:    ptr(pool.DesiredSize),
			MinCount: ptr(pool.MinSize),
			MaxCount: ptr(pool.MaxSize),
		},
	}
}
