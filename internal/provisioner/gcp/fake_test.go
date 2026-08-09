package gcp

import (
	"context"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	gax "github.com/googleapis/gax-go/v2"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	iam "google.golang.org/api/iam/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// fakeGCP stands in for the GKE, IAM, and Compute clients, recording every
// call by name so tests can assert which calls were made — which is how
// "reconcile changed nothing" is held to making no mutating calls at all.
type fakeGCP struct {
	calls []string

	cluster   *containerpb.Cluster
	nodePools map[string]*containerpb.NodePool
	svcAccts  map[string]*iam.ServiceAccount // resource -> account
	policies  map[string]*iam.Policy         // resource -> policy
	firewalls map[string]*compute.Firewall
}

func newFakeGCP() *fakeGCP {
	return &fakeGCP{
		nodePools: map[string]*containerpb.NodePool{},
		svcAccts:  map[string]*iam.ServiceAccount{},
		policies:  map[string]*iam.Policy{},
		firewalls: map[string]*compute.Firewall{},
	}
}

func (f *fakeGCP) record(name string) { f.calls = append(f.calls, name) }

var mutatingCalls = []string{
	"CreateCluster", "UpdateCluster", "DeleteCluster",
	"CreateNodePool", "SetNodePoolSize", "DeleteNodePool",
	"CreateServiceAccount", "DeleteServiceAccount", "SetIamPolicy",
	"InsertFirewall",
}

func (f *fakeGCP) assertNoMutations(t *testing.T) {
	t.Helper()
	for _, call := range f.calls {
		for _, mutator := range mutatingCalls {
			if call == mutator {
				t.Errorf("expected no cloud changes, but %s was called", call)
			}
		}
	}
}

func (f *fakeGCP) clients() *Clients {
	return &Clients{project: testProject, cluster: f, svcAccts: f, firewalls: f}
}

// --- GKE ---

func (f *fakeGCP) GetCluster(_ context.Context, req *containerpb.GetClusterRequest, _ ...gax.CallOption) (*containerpb.Cluster, error) {
	f.record("GetCluster")
	if f.cluster == nil || f.cluster.Name != clusterNameFromPath(req.Name) {
		return nil, status.Error(codes.NotFound, "cluster not found")
	}
	return f.cluster, nil
}

func (f *fakeGCP) CreateCluster(_ context.Context, req *containerpb.CreateClusterRequest, _ ...gax.CallOption) (*containerpb.Operation, error) {
	f.record("CreateCluster")
	if f.cluster != nil {
		return nil, status.Error(codes.AlreadyExists, "cluster exists")
	}
	f.cluster = req.Cluster
	f.cluster.Status = containerpb.Cluster_PROVISIONING
	for _, np := range req.Cluster.NodePools {
		f.nodePools[np.Name] = np
	}
	return &containerpb.Operation{}, nil
}

func (f *fakeGCP) UpdateCluster(_ context.Context, req *containerpb.UpdateClusterRequest, _ ...gax.CallOption) (*containerpb.Operation, error) {
	f.record("UpdateCluster")
	//nolint:staticcheck // production code still writes these deprecated fields, see cluster.go
	if req.Update.DesiredPrivateClusterConfig != nil {
		f.cluster.PrivateClusterConfig = req.Update.DesiredPrivateClusterConfig //nolint:staticcheck
	}
	if req.Update.DesiredMasterAuthorizedNetworksConfig != nil { //nolint:staticcheck
		f.cluster.MasterAuthorizedNetworksConfig = req.Update.DesiredMasterAuthorizedNetworksConfig //nolint:staticcheck
	}
	return &containerpb.Operation{}, nil
}

func (f *fakeGCP) DeleteCluster(_ context.Context, _ *containerpb.DeleteClusterRequest, _ ...gax.CallOption) (*containerpb.Operation, error) {
	f.record("DeleteCluster")
	if f.cluster == nil {
		return nil, status.Error(codes.NotFound, "cluster not found")
	}
	f.cluster = nil
	f.nodePools = map[string]*containerpb.NodePool{}
	return &containerpb.Operation{}, nil
}

func (f *fakeGCP) ListNodePools(_ context.Context, _ *containerpb.ListNodePoolsRequest, _ ...gax.CallOption) (*containerpb.ListNodePoolsResponse, error) {
	f.record("ListNodePools")
	pools := make([]*containerpb.NodePool, 0, len(f.nodePools))
	for _, np := range f.nodePools {
		pools = append(pools, np)
	}
	return &containerpb.ListNodePoolsResponse{NodePools: pools}, nil
}

func (f *fakeGCP) GetNodePool(_ context.Context, req *containerpb.GetNodePoolRequest, _ ...gax.CallOption) (*containerpb.NodePool, error) {
	f.record("GetNodePool")
	np, ok := f.nodePools[clusterNameFromPath(req.Name)]
	if !ok {
		return nil, status.Error(codes.NotFound, "node pool not found")
	}
	return np, nil
}

func (f *fakeGCP) CreateNodePool(_ context.Context, req *containerpb.CreateNodePoolRequest, _ ...gax.CallOption) (*containerpb.Operation, error) {
	f.record("CreateNodePool")
	if _, ok := f.nodePools[req.NodePool.Name]; ok {
		return nil, status.Error(codes.AlreadyExists, "node pool exists")
	}
	f.nodePools[req.NodePool.Name] = req.NodePool
	return &containerpb.Operation{}, nil
}

func (f *fakeGCP) SetNodePoolSize(_ context.Context, req *containerpb.SetNodePoolSizeRequest, _ ...gax.CallOption) (*containerpb.Operation, error) {
	f.record("SetNodePoolSize")
	np, ok := f.nodePools[clusterNameFromPath(req.Name)]
	if !ok {
		return nil, status.Error(codes.NotFound, "node pool not found")
	}
	np.InitialNodeCount = req.NodeCount
	return &containerpb.Operation{}, nil
}

func (f *fakeGCP) DeleteNodePool(_ context.Context, req *containerpb.DeleteNodePoolRequest, _ ...gax.CallOption) (*containerpb.Operation, error) {
	f.record("DeleteNodePool")
	delete(f.nodePools, clusterNameFromPath(req.Name))
	return &containerpb.Operation{}, nil
}

// --- IAM ---

func (f *fakeGCP) Get(_ context.Context, name string) (*iam.ServiceAccount, error) {
	f.record("GetServiceAccount")
	sa, ok := f.svcAccts[name]
	if !ok {
		return nil, &googleapi.Error{Code: 404}
	}
	return sa, nil
}

func (f *fakeGCP) Create(_ context.Context, _ string, req *iam.CreateServiceAccountRequest) (*iam.ServiceAccount, error) {
	f.record("CreateServiceAccount")
	sa := &iam.ServiceAccount{
		Email:       req.AccountId + "@" + testProject + ".iam.gserviceaccount.com",
		DisplayName: req.ServiceAccount.DisplayName,
	}
	resource := "projects/" + testProject + "/serviceAccounts/" + sa.Email
	if _, ok := f.svcAccts[resource]; ok {
		return nil, &googleapi.Error{Code: 409}
	}
	f.svcAccts[resource] = sa
	f.policies[resource] = &iam.Policy{}
	return sa, nil
}

func (f *fakeGCP) Delete(_ context.Context, name string) error {
	f.record("DeleteServiceAccount")
	if _, ok := f.svcAccts[name]; !ok {
		return &googleapi.Error{Code: 404}
	}
	delete(f.svcAccts, name)
	delete(f.policies, name)
	return nil
}

func (f *fakeGCP) GetIamPolicy(_ context.Context, resource string) (*iam.Policy, error) {
	f.record("GetIamPolicy")
	p, ok := f.policies[resource]
	if !ok {
		return nil, &googleapi.Error{Code: 404}
	}
	return p, nil
}

func (f *fakeGCP) SetIamPolicy(_ context.Context, resource string, req *iam.SetIamPolicyRequest) (*iam.Policy, error) {
	f.record("SetIamPolicy")
	f.policies[resource] = req.Policy
	return req.Policy, nil
}

// --- Compute ---

func (f *fakeGCP) GetFirewall(_ context.Context, _, name string) (*compute.Firewall, error) {
	f.record("GetFirewall")
	fw, ok := f.firewalls[name]
	if !ok {
		return nil, &googleapi.Error{Code: 404}
	}
	return fw, nil
}

func (f *fakeGCP) Insert(_ context.Context, _ string, fw *compute.Firewall) error {
	f.record("InsertFirewall")
	if _, ok := f.firewalls[fw.Name]; ok {
		return &googleapi.Error{Code: 409}
	}
	f.firewalls[fw.Name] = fw
	return nil
}

// --- helpers ---

const testProject = "kubespin-test"

func clusterNameFromPath(path string) string {
	// ".../clusters/{name}"
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

func testSpec() core.ClusterSpec {
	return core.ClusterSpec{
		ID:       "team-payments-prod",
		Provider: core.ProviderGCP,
		Region:   "us-central1",
		Access:   core.AccessPrivate,
		Subnets:  []string{"projects/kubespin-test/regions/us-central1/subnetworks/default"},
		NodePools: []core.NodePool{{
			Name: "default", InstanceType: "e2-standard-4", MinSize: 1, MaxSize: 5, DesiredSize: 3,
		}},
		Profile: core.ProfileRef{Name: "tier-small", Version: "1.0.0"},
	}
}

// activeCluster puts the fake into the state a successfully created cluster
// leaves behind.
func (f *fakeGCP) activeCluster(spec core.ClusterSpec) {
	f.cluster = &containerpb.Cluster{
		Name:                 spec.ID.String(),
		Status:               containerpb.Cluster_RUNNING,
		Endpoint:             "203.0.113.10",
		Network:              "default",
		CurrentMasterVersion: "1.30.1",
		WorkloadIdentityConfig: &containerpb.WorkloadIdentityConfig{
			WorkloadPool: testProject + ".svc.id.goog",
		},
		PrivateClusterConfig: &containerpb.PrivateClusterConfig{
			EnablePrivateNodes:    true,
			EnablePrivateEndpoint: spec.Access == core.AccessPrivate,
		},
	}
}

// withNodePool registers a node pool matching the given pool.
func (f *fakeGCP) withNodePool(pool core.NodePool) {
	f.nodePools[pool.Name] = &containerpb.NodePool{
		Name:             pool.Name,
		InitialNodeCount: pool.DesiredSize,
		Config:           &containerpb.NodeConfig{MachineType: pool.InstanceType, Labels: pool.Labels},
		Autoscaling: &containerpb.NodePoolAutoscaling{
			Enabled: true, MinNodeCount: pool.MinSize, MaxNodeCount: pool.MaxSize,
		},
	}
}
