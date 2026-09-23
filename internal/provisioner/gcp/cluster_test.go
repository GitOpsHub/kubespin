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

func TestClusterProvisioner_Reconcile_NodePoolBoundsDrift(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	f.activeCluster(spec)
	drifted := spec.NodePools[0]
	drifted.MaxSize = 2
	f.withNodePool(drifted)
	p := NewClusterProvisioner(f.clients())

	change, err := p.Reconcile(context.Background(), spec)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !change.Changed {
		t.Fatal("expected the bounds change to be reported as a change")
	}
	as := f.nodePools["default"].Autoscaling
	if !as.Enabled || as.MinNodeCount != spec.NodePools[0].MinSize || as.MaxNodeCount != spec.NodePools[0].MaxSize {
		t.Errorf("autoscaling = %+v, want enabled %d-%d", as, spec.NodePools[0].MinSize, spec.NodePools[0].MaxSize)
	}
}

// The live node count belongs to GKE's autoscaler. A pool whose count has
// moved away from spec's DesiredSize, but whose bounds match, is converged:
// resetting it would fight the autoscaler and report a change on every apply.
func TestClusterProvisioner_Reconcile_DesiredSizeDriftIsNotAChange(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	f.activeCluster(spec)
	scaled := spec.NodePools[0]
	scaled.DesiredSize = 1
	f.withNodePool(scaled)
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

// A zonal cluster reconciled with no zone in the spec must be addressed where
// it actually lives: before, Describe found it through locate but every
// node-pool call went to the region-derived path and failed NotFound.
func TestClusterProvisioner_Reconcile_ZonalClusterWithNoZoneInSpec(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	spec.NodePools = append(spec.NodePools, core.NodePool{
		Name: "extra", InstanceType: "e2-standard-2", MinSize: 0, MaxSize: 3, DesiredSize: 1,
	})
	f.activeCluster(spec)
	f.clusterLocation = "us-central1-b"
	f.withNodePool(spec.NodePools[0])
	p := NewClusterProvisioner(f.clients())

	if _, err := p.Reconcile(context.Background(), spec); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	np, ok := f.nodePools["extra"]
	if !ok {
		t.Fatal("expected the extra node pool to have been created")
	}
	if !slices.Equal(np.Locations, []string{"us-central1-b"}) {
		t.Errorf("node pool locations = %v, want the zonal cluster's own zone", np.Locations)
	}
}

// A zone that is set but wrong (as `delete --spot` derives one) must not make
// a live regional cluster look already gone.
func TestClusterProvisioner_Delete_FindsARegionalClusterDespiteAWrongZone(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	f.activeCluster(spec)
	f.clusterLocation = spec.Region
	spec.Zone = "us-central1-a"
	p := NewClusterProvisioner(f.clients())

	if err := p.Delete(context.Background(), spec); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if f.cluster != nil {
		t.Error("expected the regional cluster to be deleted, not read as already gone")
	}
}

// Node pools are pinned to the region's first UP zone by name, never an
// assumed "<region>-a": us-east1 has no such zone.
func TestClusterProvisioner_Create_PinsNodePoolToARealZone(t *testing.T) {
	for region, want := range map[string]string{
		"us-central1": "us-central1-a",
		"us-east1":    "us-east1-b",
	} {
		t.Run(region, func(t *testing.T) {
			f := newFakeGCP()
			f.subnetworks[region+"/default"] = f.subnetworks["us-central1/default"]
			spec := testSpec()
			spec.Region = region
			p := NewClusterProvisioner(f.clients())

			if err := p.Create(context.Background(), spec); err != nil {
				t.Fatalf("Create: %v", err)
			}
			got := f.cluster.NodePools[0].Locations
			if !slices.Equal(got, []string{want}) {
				t.Errorf("node pool locations = %v, want [%s]", got, want)
			}
		})
	}
}

func TestClusterProvisioner_Create_FailsWhenRegionHasNoZones(t *testing.T) {
	f := newFakeGCP()
	f.subnetworks["nowhere1/default"] = f.subnetworks["us-central1/default"]
	spec := testSpec()
	spec.Region = "nowhere1"

	err := NewClusterProvisioner(f.clients()).Create(context.Background(), spec)
	if err == nil {
		t.Fatal("expected an error for a region with no zones")
	}
	if f.cluster != nil {
		t.Error("expected no cluster to be requested")
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

func TestClusterProvisioner_Create_Autopilot_SetsAutopilotEnabled(t *testing.T) {
	f := newFakeGCP()
	p := NewClusterProvisioner(f.clients())
	spec := testSpec()
	spec.Autopilot = true
	spec.NodePools = nil

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if f.cluster == nil {
		t.Fatal("expected a cluster to have been created")
	}
	if !f.cluster.GetAutopilot().GetEnabled() {
		t.Error("expected Autopilot.Enabled to be true")
	}
	if len(f.cluster.NodePools) != 0 {
		t.Errorf("NodePools = %v, want none set under Autopilot", f.cluster.NodePools)
	}
}

func TestClusterProvisioner_Create_Autopilot_SkipsNodePoolCreation(t *testing.T) {
	f := newFakeGCP()
	p := NewClusterProvisioner(f.clients())
	spec := testSpec()
	spec.Autopilot = true
	spec.NodePools = nil
	f.activeCluster(spec)

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, call := range f.calls {
		if call == "CreateNodePool" || call == "SetNodePoolAutoscaling" {
			t.Errorf("unexpected node-pool call %q under Autopilot", call)
		}
	}
}

func TestClusterProvisioner_Reconcile_Autopilot_SkipsEnsureNodePools(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	spec.Autopilot = true
	spec.NodePools = nil
	f.activeCluster(spec)
	f.calls = nil

	state, err := NewClusterProvisioner(f.clients()).Reconcile(context.Background(), spec)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if state.Changed {
		t.Errorf("expected no change under Autopilot, got %v", state.Details)
	}
	for _, call := range f.calls {
		if call == "CreateNodePool" || call == "SetNodePoolAutoscaling" {
			t.Errorf("unexpected node-pool call %q under Autopilot", call)
		}
	}
}

// A `delete` invocation rebuilds ClusterSpec from flags with no --spec file,
// so it will not necessarily have --autopilot set even for a cluster that
// was created with it. Describe must detect Autopilot from the live cluster
// itself, not trust the caller's spec, so Delete/Reconcile still work.
func TestClusterProvisioner_Describe_DetectsAutopilotFromLiveCluster(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	spec.Autopilot = true
	f.activeCluster(spec)

	// Simulate a `delete` invocation: the spec passed in has no --autopilot,
	// unlike the one that created the cluster.
	describeSpec := spec
	describeSpec.Autopilot = false

	state, err := NewClusterProvisioner(f.clients()).Describe(context.Background(), describeSpec)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !state.Autopilot {
		t.Error("expected Describe to detect Autopilot from the live cluster, regardless of spec.Autopilot")
	}
}

func TestClusterProvisioner_Delete_DoesNotRequireAutopilotFlag(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	spec.Autopilot = true
	spec.NodePools = nil
	f.activeCluster(spec)

	// The delete-side spec omits --autopilot, as a real `delete` invocation
	// without a --spec file would.
	deleteSpec := spec
	deleteSpec.Autopilot = false

	if err := NewClusterProvisioner(f.clients()).Delete(context.Background(), deleteSpec); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if slices.Contains(f.calls, "ListNodePools") {
		t.Error("Delete should not list node pools for a live Autopilot cluster, even without --autopilot on the spec")
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
