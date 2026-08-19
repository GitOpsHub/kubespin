package gcp

import (
	"context"
	"slices"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

func TestClusterProvisioner_Create_NewCluster(t *testing.T) {
	f := newFakeGCP()
	p := NewClusterProvisioner(f.clients())
	spec := testSpec()

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if f.cluster == nil {
		t.Fatal("expected a cluster to have been created")
	}
	if f.cluster.Name != spec.ID.String() {
		t.Errorf("cluster name = %q, want %q", f.cluster.Name, spec.ID.String())
	}
	if !f.cluster.PrivateClusterConfig.EnablePrivateEndpoint { //nolint:staticcheck // production code still writes this field, see cluster.go
		t.Error("expected a private endpoint for an AccessPrivate spec")
	}
	if f.cluster.WorkloadIdentityConfig.WorkloadPool != testProject+".svc.id.goog" {
		t.Errorf("workload pool = %q", f.cluster.WorkloadIdentityConfig.WorkloadPool)
	}
}

func TestClusterProvisioner_Create_RegionalByDefault(t *testing.T) {
	f := newFakeGCP()
	p := NewClusterProvisioner(f.clients())
	spec := testSpec()

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := "projects/" + testProject + "/locations/" + spec.Region
	if f.createParent != want {
		t.Errorf("CreateCluster parent = %q, want %q (regional)", f.createParent, want)
	}
}

func TestClusterProvisioner_Create_ZonalWhenZoneSet(t *testing.T) {
	f := newFakeGCP()
	p := NewClusterProvisioner(f.clients())
	spec := testSpec()
	spec.Zone = "us-central1-a"

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := "projects/" + testProject + "/locations/" + spec.Zone
	if f.createParent != want {
		t.Errorf("CreateCluster parent = %q, want %q (zonal)", f.createParent, want)
	}
}

func TestClusterProvisioner_Create_SpotNodePool(t *testing.T) {
	f := newFakeGCP()
	p := NewClusterProvisioner(f.clients())
	spec := testSpec()
	spec.NodePools[0].CapacityType = core.CapacityTypeSpot
	f.activeCluster(spec)

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	np, ok := f.nodePools["default"]
	if !ok {
		t.Fatal("expected the default node pool to have been created")
	}
	if !np.Config.Spot {
		t.Error("expected the node pool's Config.Spot to be true")
	}
}

func TestClusterProvisioner_Create_PublicNodesSkipsPrivateNodes(t *testing.T) {
	f := newFakeGCP()
	p := NewClusterProvisioner(f.clients())
	spec := testSpec()
	spec.PublicNodes = true

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if f.cluster.PrivateClusterConfig.EnablePrivateNodes { //nolint:staticcheck // production code still writes this field, see cluster.go
		t.Error("expected EnablePrivateNodes to be false when PublicNodes is set")
	}
}

func TestClusterProvisioner_Create_Idempotent(t *testing.T) {
	f := newFakeGCP()
	p := NewClusterProvisioner(f.clients())
	spec := testSpec()

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("second Create should converge, not fail: %v", err)
	}
}

func TestClusterProvisioner_Create_AttachesNodePoolsOnceActive(t *testing.T) {
	f := newFakeGCP()
	p := NewClusterProvisioner(f.clients())
	spec := testSpec()
	f.activeCluster(spec)

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, ok := f.nodePools["default"]; !ok {
		t.Error("expected the default node pool to have been created")
	}
}

func TestClusterProvisioner_Describe_Absent(t *testing.T) {
	f := newFakeGCP()
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
	f := newFakeGCP()
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
	if len(state.NodePools) != 1 || state.NodePools[0].Name != "default" {
		t.Errorf("node pools = %+v", state.NodePools)
	}
}

func TestClusterProvisioner_Reconcile_NoDrift_MakesNoMutatingCalls(t *testing.T) {
	f := newFakeGCP()
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
	f := newFakeGCP()
	spec := testSpec()
	spec.Access = core.AccessPublic
	f.activeCluster(spec)
	f.cluster.PrivateClusterConfig.EnablePrivateEndpoint = true //nolint:staticcheck // drifted to private; see cluster.go
	f.withNodePool(spec.NodePools[0])
	p := NewClusterProvisioner(f.clients())

	change, err := p.Reconcile(context.Background(), spec)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !change.Changed {
		t.Fatal("expected access drift to be reported as a change")
	}
	if f.cluster.PrivateClusterConfig.EnablePrivateEndpoint { //nolint:staticcheck // see cluster.go
		t.Error("expected the endpoint to have been made public")
	}
}

func TestClusterProvisioner_Reconcile_NodePoolResize(t *testing.T) {
	f := newFakeGCP()
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
	if f.nodePools["default"].InitialNodeCount != spec.NodePools[0].DesiredSize {
		t.Errorf("desired size = %d, want %d",
			f.nodePools["default"].InitialNodeCount, spec.NodePools[0].DesiredSize)
	}
}

func TestClusterProvisioner_Reconcile_NewNodePool(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	spec.NodePools = append(spec.NodePools, core.NodePool{
		Name: "spot", InstanceType: "e2-standard-2", MinSize: 0, MaxSize: 3, DesiredSize: 1,
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
	if _, ok := f.nodePools["spot"]; !ok {
		t.Error("expected the spot node pool to have been created")
	}
}

func TestClusterProvisioner_Reconcile_AbsentClusterErrors(t *testing.T) {
	f := newFakeGCP()
	p := NewClusterProvisioner(f.clients())

	if _, err := p.Reconcile(context.Background(), testSpec()); err == nil {
		t.Fatal("expected an error reconciling an absent cluster")
	}
}

func TestClusterProvisioner_Delete(t *testing.T) {
	f := newFakeGCP()
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

// A cluster created zonal (spec.Zone set, as --spot does automatically) must
// still be found and actually deleted even when the caller's spec carries no
// Zone — exactly what happens when `delete` is invoked without re-supplying
// --zone/--spot, which its own flag help documents as optional. Before
// locate existed, this silently no-op'd: DeleteCluster addressed the wrong
// (region-derived) path, got NotFound, and Delete treated that as "already
// gone" while the real cluster kept running.
func TestClusterProvisioner_Delete_FindsAZonalClusterWithNoZoneInSpec(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	spec.Zone = "" // the caller does not know the cluster is zonal
	f.activeCluster(spec)
	f.clusterLocation = "us-central1-a" // ...but this is where it actually lives
	p := NewClusterProvisioner(f.clients())

	if err := p.Delete(context.Background(), spec); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if f.cluster != nil {
		t.Error("expected the zonal cluster to be deleted, not silently skipped")
	}
	if !slices.Contains(f.calls, "ListClusters") {
		t.Error("expected Delete to fall back to a project-wide search after the region-derived path 404s")
	}
}

// The teardown a retried `delete` resumes runs against a cluster GKE is still
// tearing down; a second DeleteCluster there fails with FailedPrecondition.
func TestClusterProvisioner_Delete_ConvergesOnAClusterAlreadyDeleting(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	f.activeCluster(spec)
	f.cluster.Status = containerpb.Cluster_STOPPING
	f.calls = nil

	if err := NewClusterProvisioner(f.clients()).Delete(context.Background(), spec); err != nil {
		t.Fatalf("Delete on a deleting cluster: %v", err)
	}
	if slices.Contains(f.calls, "DeleteCluster") {
		t.Errorf("calls = %v, want no second DeleteCluster while one is in flight", f.calls)
	}
}

func TestNormaliseStatus(t *testing.T) {
	cases := map[containerpb.Cluster_Status]provisioner.Status{
		containerpb.Cluster_RUNNING:      provisioner.StatusActive,
		containerpb.Cluster_PROVISIONING: provisioner.StatusCreating,
		containerpb.Cluster_RECONCILING:  provisioner.StatusUpdating,
		containerpb.Cluster_STOPPING:     provisioner.StatusDeleting,
		containerpb.Cluster_ERROR:        provisioner.StatusFailed,
		containerpb.Cluster_DEGRADED:     provisioner.StatusFailed,
	}
	for in, want := range cases {
		if got := normaliseStatus(in); got != want {
			t.Errorf("normaliseStatus(%v) = %v, want %v", in, got, want)
		}
	}
}
