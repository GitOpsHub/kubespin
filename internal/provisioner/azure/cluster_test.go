package azure

import (
	"context"
	"slices"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v6"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

func TestClusterProvisioner_Create_NewCluster(t *testing.T) {
	f := newFakeAzure()
	p := NewClusterProvisioner(f.clients())
	spec := testSpec()

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if f.cluster == nil {
		t.Fatal("expected a cluster to have been created")
	}
	if deref(f.cluster.Name) != spec.ID.String() {
		t.Errorf("cluster name = %q, want %q", deref(f.cluster.Name), spec.ID.String())
	}
	if !*f.cluster.Properties.APIServerAccessProfile.EnablePrivateCluster {
		t.Error("expected a private cluster for an AccessPrivate spec")
	}
	if !*f.cluster.Properties.SecurityProfile.WorkloadIdentity.Enabled {
		t.Error("expected workload identity to be enabled")
	}
	if f.cluster.SKU == nil || f.cluster.SKU.Tier == nil || *f.cluster.SKU.Tier != armcontainerservice.ManagedClusterSKUTierFree {
		t.Errorf("SKU tier = %v, want %s (no control-plane charge)", f.cluster.SKU, armcontainerservice.ManagedClusterSKUTierFree)
	}
}

// A supplied (or kubespin-created) subnet must actually be wired onto the
// agent pool — AKS otherwise silently falls back to its own default network.
func TestClusterProvisioner_Create_SetsVnetSubnetID(t *testing.T) {
	f := newFakeAzure()
	p := NewClusterProvisioner(f.clients())
	spec := testSpec()

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	pool, ok := f.agentPools[spec.NodePools[0].Name]
	if !ok {
		t.Fatal("expected the default node pool to have been created")
	}
	if pool.Properties.VnetSubnetID == nil || *pool.Properties.VnetSubnetID != spec.Subnets[0] {
		t.Errorf("VnetSubnetID = %v, want %q", pool.Properties.VnetSubnetID, spec.Subnets[0])
	}
}

func TestClusterProvisioner_Create_Idempotent(t *testing.T) {
	f := newFakeAzure()
	p := NewClusterProvisioner(f.clients())
	spec := testSpec()

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("second Create should converge, not fail: %v", err)
	}
}

func TestClusterProvisioner_Describe_Absent(t *testing.T) {
	f := newFakeAzure()
	p := NewClusterProvisioner(f.clients())

	state, err := p.Describe(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if state.Status != provisioner.StatusAbsent {
		t.Errorf("status = %v, want StatusAbsent", state.Status)
	}
}

func TestClusterProvisioner_Describe_Active(t *testing.T) {
	f := newFakeAzure()
	spec := testSpec()
	f.activeCluster(spec)
	f.withNodePool(spec.NodePools[0])
	p := NewClusterProvisioner(f.clients())

	state, err := p.Describe(context.Background(), spec)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if state.Status != provisioner.StatusActive {
		t.Errorf("status = %v, want StatusActive", state.Status)
	}
	if state.Access != core.AccessPrivate {
		t.Errorf("access = %v, want private", state.Access)
	}
	if state.OIDCIssuer != testIssuer {
		t.Errorf("issuer = %q, want %q", state.OIDCIssuer, testIssuer)
	}
	if len(state.NodePools) != 1 || state.NodePools[0].Name != "default" {
		t.Errorf("node pools = %+v", state.NodePools)
	}
}

func TestClusterProvisioner_Reconcile_NoDrift_MakesNoMutatingCalls(t *testing.T) {
	f := newFakeAzure()
	spec := testSpec()
	f.activeCluster(spec)
	f.withNodePool(spec.NodePools[0])
	p := NewClusterProvisioner(f.clients())

	change, err := p.Reconcile(context.Background(), spec)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if change.Changed {
		t.Errorf("expected no change, got %+v", change)
	}
	f.assertNoMutations(t)
}

func TestClusterProvisioner_Reconcile_AccessDrift(t *testing.T) {
	f := newFakeAzure()
	spec := testSpec()
	spec.Access = core.AccessPublic
	f.activeCluster(spec)
	f.cluster.Properties.APIServerAccessProfile.EnablePrivateCluster = ptr(true) // drifted to private
	f.withNodePool(spec.NodePools[0])
	p := NewClusterProvisioner(f.clients())

	change, err := p.Reconcile(context.Background(), spec)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !change.Changed {
		t.Fatal("expected access drift to be reported as a change")
	}
	if *f.cluster.Properties.APIServerAccessProfile.EnablePrivateCluster {
		t.Error("expected the cluster to have been made public")
	}
}

func TestClusterProvisioner_Reconcile_NodePoolResize(t *testing.T) {
	f := newFakeAzure()
	spec := testSpec()
	f.activeCluster(spec)
	drifted := spec.NodePools[0]
	drifted.DesiredSize = 1
	f.withNodePool(drifted)
	p := NewClusterProvisioner(f.clients())

	change, err := p.Reconcile(context.Background(), spec)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !change.Changed {
		t.Fatal("expected the resize to be reported as a change")
	}
	if derefInt32(f.agentPools["default"].Properties.Count) != spec.NodePools[0].DesiredSize {
		t.Errorf("desired size = %d, want %d",
			derefInt32(f.agentPools["default"].Properties.Count), spec.NodePools[0].DesiredSize)
	}
}

func TestClusterProvisioner_Reconcile_NewNodePool(t *testing.T) {
	f := newFakeAzure()
	spec := testSpec()
	spec.NodePools = append(spec.NodePools, core.NodePool{
		Name: "spot", InstanceType: "Standard_D2s_v5", MinSize: 0, MaxSize: 3, DesiredSize: 1,
	})
	f.activeCluster(spec)
	f.withNodePool(testSpec().NodePools[0])
	p := NewClusterProvisioner(f.clients())

	change, err := p.Reconcile(context.Background(), spec)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !change.Changed {
		t.Fatal("expected creating a node pool to be reported as a change")
	}
	if _, ok := f.agentPools["spot"]; !ok {
		t.Error("expected the spot node pool to have been created")
	}
}

func TestClusterProvisioner_Reconcile_AbsentClusterErrors(t *testing.T) {
	f := newFakeAzure()
	p := NewClusterProvisioner(f.clients())

	if _, err := p.Reconcile(context.Background(), testSpec()); err == nil {
		t.Fatal("expected an error reconciling an absent cluster")
	}
}

func TestClusterProvisioner_Delete(t *testing.T) {
	f := newFakeAzure()
	spec := testSpec()
	f.activeCluster(spec)
	f.withNodePool(spec.NodePools[0])
	p := NewClusterProvisioner(f.clients())

	if err := p.Delete(context.Background(), spec); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if f.cluster != nil {
		t.Error("expected the cluster to be gone")
	}

	// Deleting an absent cluster converges rather than erroring.
	if err := p.Delete(context.Background(), spec); err != nil {
		t.Fatalf("second Delete should converge: %v", err)
	}
}

// The teardown a retried `delete` resumes runs against a cluster Azure is
// still tearing down; a second delete there comes back 409.
func TestClusterProvisioner_Delete_ConvergesOnAClusterAlreadyDeleting(t *testing.T) {
	f := newFakeAzure()
	spec := testSpec()
	f.activeCluster(spec)
	f.cluster.Properties.ProvisioningState = ptr("Deleting")
	f.calls = nil

	if err := NewClusterProvisioner(f.clients()).Delete(context.Background(), spec); err != nil {
		t.Fatalf("Delete on a deleting cluster: %v", err)
	}
	if slices.Contains(f.calls, "DeleteCluster") {
		t.Errorf("calls = %v, want no second delete while one is in flight", f.calls)
	}
}

func TestNormaliseStatus(t *testing.T) {
	cases := map[string]provisioner.Status{
		"Succeeded": provisioner.StatusActive,
		"Creating":  provisioner.StatusCreating,
		"Updating":  provisioner.StatusUpdating,
		"Deleting":  provisioner.StatusDeleting,
		"Failed":    provisioner.StatusFailed,
		"Canceled":  provisioner.StatusFailed,
	}
	for in, want := range cases {
		if got := normaliseStatus(in); got != want {
			t.Errorf("normaliseStatus(%v) = %v, want %v", in, got, want)
		}
	}
}
