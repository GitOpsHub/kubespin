package aws

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

func TestCreate_RequestsAClusterAndItsServiceRole(t *testing.T) {
	f := newFakeAWS()
	p := NewClusterProvisioner(f.clients())

	if err := p.Create(t.Context(), testSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if !f.called("CreateRole") {
		t.Error("no cluster service role was created")
	}
	if !f.called("CreateCluster") {
		t.Error("CreateCluster was never called")
	}
	if got := f.attached["kubespin-team-payments-prod-cluster"]; len(got) != 1 || got[0] != policyEKSCluster {
		t.Errorf("cluster role policies = %v, want just %s", got, policyEKSCluster)
	}

	// Node groups cannot attach until the control plane is active, so Create
	// must not attempt them yet.
	if f.called("CreateNodegroup") {
		t.Error("node groups were created before the control plane was active")
	}
}

// The access mode has to reach the cloud correctly in both directions; getting
// this wrong is how a cluster ends up unintentionally reachable.
func TestCreate_AccessModeBranching(t *testing.T) {
	t.Run("private has no public endpoint", func(t *testing.T) {
		f := newFakeAWS()
		spec := testSpec()

		if err := NewClusterProvisioner(f.clients()).Create(t.Context(), spec); err != nil {
			t.Fatalf("Create: %v", err)
		}

		cfg := f.cluster.ResourcesVpcConfig
		if cfg.EndpointPublicAccess {
			t.Error("a private cluster was created with a public endpoint")
		}
		if !cfg.EndpointPrivateAccess {
			t.Error("the private endpoint was not enabled")
		}
	})

	t.Run("public is restricted to the authorized CIDRs", func(t *testing.T) {
		f := newFakeAWS()
		spec := testSpec()
		spec.Access = core.AccessPublic
		spec.AuthorizedCIDRs = []string{"203.0.113.0/24"}

		if err := NewClusterProvisioner(f.clients()).Create(t.Context(), spec); err != nil {
			t.Fatalf("Create: %v", err)
		}

		cfg := f.cluster.ResourcesVpcConfig
		if !cfg.EndpointPublicAccess {
			t.Error("a public cluster was created without a public endpoint")
		}
		if len(cfg.PublicAccessCidrs) != 1 || cfg.PublicAccessCidrs[0] != "203.0.113.0/24" {
			t.Errorf("PublicAccessCidrs = %v, want the spec's authorized CIDRs", cfg.PublicAccessCidrs)
		}
		// Private access stays on so in-VPC traffic never leaves the network.
		if !cfg.EndpointPrivateAccess {
			t.Error("the private endpoint was disabled on a public cluster")
		}
	})
}

func TestCreate_IsIdempotent(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	f.activeCluster(spec)

	p := NewClusterProvisioner(f.clients())
	if err := p.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if f.called("CreateCluster") {
		t.Error("CreateCluster was called for a cluster that already exists")
	}
	// An active cluster with no node groups gets them, so a resumed run
	// completes rather than stalling.
	if !f.called("CreateNodegroup") {
		t.Error("node groups were not created for an active cluster")
	}
}

func TestCreate_RequestsSpotCapacityWhenAsked(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	spec.NodePools[0].CapacityType = core.CapacityTypeSpot
	f.activeCluster(spec)

	if err := NewClusterProvisioner(f.clients()).Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ng, ok := f.nodeGroups[names{spec}.nodeGroup(spec.NodePools[0].Name)]
	if !ok {
		t.Fatalf("node group %s was not created", spec.NodePools[0].Name)
	}
	if ng.CapacityType != ekstypes.CapacityTypesSpot {
		t.Errorf("CapacityType = %s, want %s", ng.CapacityType, ekstypes.CapacityTypesSpot)
	}
}

func TestCreate_DefaultsToOnDemandCapacity(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	f.activeCluster(spec)

	if err := NewClusterProvisioner(f.clients()).Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ng, ok := f.nodeGroups[names{spec}.nodeGroup(spec.NodePools[0].Name)]
	if !ok {
		t.Fatalf("node group %s was not created", spec.NodePools[0].Name)
	}
	if ng.CapacityType == ekstypes.CapacityTypesSpot {
		t.Error("CapacityType = spot, want on-demand (the zero value) when --spot is not requested")
	}
}

func TestCreate_RejectsASingleSubnet(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	spec.Subnets = []string{"subnet-only-one"}

	err := NewClusterProvisioner(f.clients()).Create(t.Context(), spec)
	if !errors.Is(err, core.ErrInvalidSpec) {
		t.Fatalf("error = %v, want one wrapping ErrInvalidSpec", err)
	}
	f.assertNoMutations(t)
}

func TestDescribe(t *testing.T) {
	t.Run("absent is not an error", func(t *testing.T) {
		// Polling has to tolerate "not there yet" as a normal answer.
		f := newFakeAWS()

		state, err := NewClusterProvisioner(f.clients()).Describe(t.Context(), testSpec())
		if err != nil {
			t.Fatalf("Describe: %v", err)
		}
		if state.Status != provisioner.StatusAbsent {
			t.Errorf("Status = %s, want absent", state.Status)
		}
	})

	t.Run("active reports issuer, security group, and pools", func(t *testing.T) {
		f := newFakeAWS()
		spec := testSpec()
		f.activeCluster(spec)
		f.withNodePool(spec, spec.NodePools[0])

		state, err := NewClusterProvisioner(f.clients()).Describe(t.Context(), spec)
		if err != nil {
			t.Fatalf("Describe: %v", err)
		}

		if state.Status != provisioner.StatusActive {
			t.Errorf("Status = %s, want active", state.Status)
		}
		if state.OIDCIssuer != testIssuer {
			t.Errorf("OIDCIssuer = %q, want %q", state.OIDCIssuer, testIssuer)
		}
		if state.NetworkID != "sg-cluster" {
			t.Errorf("NetworkID = %q, want the cluster security group", state.NetworkID)
		}
		if state.Access != core.AccessPrivate {
			t.Errorf("Access = %s, want private", state.Access)
		}
		if len(state.NodePools) != 1 || state.NodePools[0].Name != "default" {
			t.Errorf("NodePools = %+v, want the pool name stripped of its cluster prefix", state.NodePools)
		}
	})

	t.Run("status normalisation", func(t *testing.T) {
		for status, want := range map[ekstypes.ClusterStatus]provisioner.Status{
			ekstypes.ClusterStatusActive:   provisioner.StatusActive,
			ekstypes.ClusterStatusCreating: provisioner.StatusCreating,
			ekstypes.ClusterStatusUpdating: provisioner.StatusUpdating,
			ekstypes.ClusterStatusDeleting: provisioner.StatusDeleting,
			ekstypes.ClusterStatusFailed:   provisioner.StatusFailed,
			ekstypes.ClusterStatusPending:  provisioner.StatusCreating,
		} {
			if got := normaliseStatus(status); got != want {
				t.Errorf("normaliseStatus(%s) = %s, want %s", status, got, want)
			}
		}
	})
}

// The property `apply` depends on: when nothing differs, nothing is called.
func TestReconcile_NoDriftMakesNoCalls(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	f.activeCluster(spec)
	f.withNodePool(spec, spec.NodePools[0])
	f.roles[names{spec}.nodeRole()] = "arn:aws:iam::123456789012:role/" + names{spec}.nodeRole()
	f.attached[names{spec}.nodeRole()] = []string{policyEKSWorkerNode, policyEKSCNI, policyECRReadOnly}
	f.calls = nil

	change, err := NewClusterProvisioner(f.clients()).Reconcile(t.Context(), spec)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if change.Changed {
		t.Errorf("Changed = true with details %v, want no change", change.Details)
	}
	f.assertNoMutations(t)
}

func TestReconcile_Drift(t *testing.T) {
	tests := map[string]struct {
		mutate   func(*core.ClusterSpec)
		wantCall string
	}{
		"node pool resized": {
			mutate:   func(s *core.ClusterSpec) { s.NodePools[0].DesiredSize = 5 },
			wantCall: "UpdateNodegroupConfig",
		},
		"node pool added": {
			mutate: func(s *core.ClusterSpec) {
				s.NodePools = append(s.NodePools, core.NodePool{
					Name: "spot", InstanceType: "m6i.xlarge", MinSize: 0, MaxSize: 4, DesiredSize: 1,
				})
			},
			wantCall: "CreateNodegroup",
		},
		"access mode changed": {
			mutate:   func(s *core.ClusterSpec) { s.Access = core.AccessPublic },
			wantCall: "UpdateClusterConfig",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFakeAWS()
			spec := testSpec()
			f.activeCluster(spec)
			f.withNodePool(spec, spec.NodePools[0])
			f.roles[names{spec}.nodeRole()] = "arn:aws:iam::123456789012:role/node"
			f.attached[names{spec}.nodeRole()] = []string{policyEKSWorkerNode, policyEKSCNI, policyECRReadOnly}

			desired := testSpec()
			tc.mutate(&desired)
			f.calls = nil

			p := NewClusterProvisioner(f.clients())
			change, err := p.Reconcile(t.Context(), desired)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			if !change.Changed {
				t.Error("Changed = false, want the drift reported")
			}
			if len(change.Details) == 0 {
				t.Error("no details explaining what changed")
			}
			if !f.called(tc.wantCall) {
				t.Errorf("%s was never called; calls were %v", tc.wantCall, f.calls)
			}

			// And the repair converges: a second reconcile finds nothing.
			f.calls = nil
			again, err := p.Reconcile(t.Context(), desired)
			if err != nil {
				t.Fatalf("second Reconcile: %v", err)
			}
			if again.Changed {
				t.Errorf("second reconcile still reports changes: %v", again.Details)
			}
		})
	}
}

func TestReconcile_MissingCluster(t *testing.T) {
	f := newFakeAWS()

	_, err := NewClusterProvisioner(f.clients()).Reconcile(t.Context(), testSpec())
	if !errors.Is(err, provisioner.ErrNotFound) {
		t.Fatalf("error = %v, want one wrapping ErrNotFound", err)
	}
}

// Removing a node pool evicts running workloads. That is a human decision, not
// something a reconcile loop should do because a file changed.
func TestReconcile_NeverDeletesNodePools(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	f.activeCluster(spec)
	f.withNodePool(spec, spec.NodePools[0])
	f.withNodePool(spec, core.NodePool{Name: "legacy", InstanceType: "m5.large", MinSize: 1, MaxSize: 2, DesiredSize: 1})
	f.roles[names{spec}.nodeRole()] = "arn:aws:iam::123456789012:role/node"
	f.attached[names{spec}.nodeRole()] = []string{policyEKSWorkerNode, policyEKSCNI, policyECRReadOnly}
	f.calls = nil

	if _, err := NewClusterProvisioner(f.clients()).Reconcile(t.Context(), spec); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if f.called("DeleteNodegroup") {
		t.Error("a node pool absent from the spec was deleted")
	}
}

func TestDelete(t *testing.T) {
	t.Run("removes node groups before the cluster", func(t *testing.T) {
		// EKS refuses to delete a cluster that still has node groups attached.
		f := newFakeAWS()
		spec := testSpec()
		f.activeCluster(spec)
		f.withNodePool(spec, spec.NodePools[0])

		if err := NewClusterProvisioner(f.clients()).Delete(t.Context(), spec); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		var deletedNodeGroup, deletedCluster int
		for i, call := range f.calls {
			switch call {
			case "DeleteNodegroup":
				deletedNodeGroup = i
			case "DeleteCluster":
				deletedCluster = i
			}
		}
		if deletedNodeGroup == 0 || deletedCluster == 0 || deletedNodeGroup > deletedCluster {
			t.Errorf("calls were %v, want the node group deleted before the cluster", f.calls)
		}
	})

	t.Run("waits for node groups to drain before deleting the cluster", func(t *testing.T) {
		// DeleteNodegroup only accepts the request; until the nodes are really
		// gone EKS answers DeleteCluster with ResourceInUseException.
		f := newFakeAWS()
		spec := testSpec()
		f.activeCluster(spec)
		f.withNodePool(spec, spec.NodePools[0])
		f.nodeGroupDeletePolls = 3

		p := NewClusterProvisioner(f.clients())
		p.wait = provisioner.WaitOptions{Interval: time.Millisecond, Timeout: time.Second}

		if err := p.Delete(t.Context(), spec); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if len(f.nodeGroups) != 0 {
			t.Fatalf("node groups still present: %v", f.nodeGroups)
		}

		// The cluster must not be deleted until a poll has seen zero node groups.
		deleteCluster := slices.Index(f.calls, "DeleteCluster")
		if deleteCluster < 0 {
			t.Fatalf("calls were %v, want the cluster deleted", f.calls)
		}
		polls := 0
		for _, call := range f.calls[:deleteCluster] {
			if call == "ListNodegroups" {
				polls++
			}
		}
		if polls < 4 { // the initial list plus one per drain poll
			t.Errorf("calls were %v, want DeleteCluster only after polling node groups to zero", f.calls)
		}
	})

	t.Run("times out rather than hanging when node groups never drain", func(t *testing.T) {
		f := newFakeAWS()
		spec := testSpec()
		f.activeCluster(spec)
		f.withNodePool(spec, spec.NodePools[0])
		f.nodeGroupDeletePolls = 1_000_000

		p := NewClusterProvisioner(f.clients())
		p.wait = provisioner.WaitOptions{Interval: time.Millisecond, Timeout: 10 * time.Millisecond}

		err := p.Delete(t.Context(), spec)
		if err == nil {
			t.Fatal("Delete succeeded, want a timeout")
		}
		if f.called("DeleteCluster") {
			t.Error("the cluster was deleted while node groups were still attached")
		}
	})

	// The teardown a retried `delete` resumes runs against a cluster EKS is
	// still tearing down; deleting it again answers ResourceInUseException.
	t.Run("converges on a cluster already deleting", func(t *testing.T) {
		f := newFakeAWS()
		spec := testSpec()
		f.activeCluster(spec)
		f.cluster.Status = ekstypes.ClusterStatusDeleting
		f.calls = nil

		if err := NewClusterProvisioner(f.clients()).Delete(t.Context(), spec); err != nil {
			t.Fatalf("Delete on a deleting cluster: %v", err)
		}
		if f.called("DeleteCluster", "DeleteNodegroup") {
			t.Errorf("calls = %v, want no second teardown request while one is in flight", f.calls)
		}
	})

	t.Run("deleting an absent cluster converges", func(t *testing.T) {
		f := newFakeAWS()

		if err := NewClusterProvisioner(f.clients()).Delete(t.Context(), testSpec()); err != nil {
			t.Fatalf("Delete on an absent cluster: %v", err)
		}
	})
}

func TestPoolNameRoundTrip(t *testing.T) {
	spec := testSpec()
	nodeGroup := names{spec}.nodeGroup("default")

	if got := poolNameFromNodeGroup(spec, nodeGroup); got != "default" {
		t.Errorf("poolNameFromNodeGroup(%q) = %q, want default", nodeGroup, got)
	}
}

func TestVpcConfig_PrivateIgnoresAuthorizedCIDRs(t *testing.T) {
	// A private cluster has no public endpoint to restrict; sending CIDRs
	// anyway would imply otherwise.
	spec := testSpec()
	spec.AuthorizedCIDRs = []string{"10.0.0.0/8"}

	cfg := vpcConfig(spec)
	if len(cfg.PublicAccessCidrs) != 0 {
		t.Errorf("PublicAccessCidrs = %v, want none for a private cluster", cfg.PublicAccessCidrs)
	}
	if aws.ToBool(cfg.EndpointPublicAccess) {
		t.Error("a private cluster requested a public endpoint")
	}
}
