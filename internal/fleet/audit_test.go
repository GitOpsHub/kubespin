package fleet

import (
	"context"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
	"github.com/GitOpsHub/kubespin/internal/repo"
)

// fakeCluster is a minimal provisioner.ClusterProvisioner for audit tests:
// Audit only ever calls Describe.
type fakeCluster struct {
	state provisioner.ClusterState
}

func (f fakeCluster) Provider() core.Provider                        { return core.ProviderAWS }
func (f fakeCluster) Create(context.Context, core.ClusterSpec) error { return nil }
func (f fakeCluster) Describe(context.Context, core.ClusterSpec) (provisioner.ClusterState, error) {
	return f.state, nil
}
func (f fakeCluster) Reconcile(context.Context, core.ClusterSpec) (provisioner.Change, error) {
	return provisioner.Change{}, nil
}
func (f fakeCluster) Delete(context.Context, core.ClusterSpec) error { return nil }

func auditTestSpec(id core.ClusterID) core.ClusterSpec {
	return core.ClusterSpec{
		ID: id, Provider: core.ProviderAWS, Region: "us-east-1", Access: core.AccessPrivate,
		Subnets: []string{"subnet-aaa"},
		NodePools: []core.NodePool{
			{Name: "default", InstanceType: "m6i.large", MinSize: 1, MaxSize: 5, DesiredSize: 3},
		},
		Size: core.SizeSmall,
	}
}

func seedRepoWithClusterYAML(t *testing.T, spec core.ClusterSpec) repo.Provisioner {
	t.Helper()

	rp := repo.NewMemory()
	if err := rp.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	checkout, err := rp.Clone(context.Background(), spec)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	clusterYAML, _, err := repo.Render(spec, core.Profile{
		Name:   "small",
		Addons: []core.AddonRef{{Name: "x", Chart: "x", Repository: "https://x", Version: "1.0.0", Namespace: "x"}},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, err := rp.Push(context.Background(), checkout, map[string][]byte{repo.ClusterFile: clusterYAML}, "seed"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	return rp
}

func TestAuditOne_NoDrift_NoFindings(t *testing.T) {
	spec := auditTestSpec("team-payments-prod")
	rp := seedRepoWithClusterYAML(t, spec)

	cluster := fakeCluster{state: provisioner.ClusterState{
		Status: provisioner.StatusActive, Access: spec.Access, NodePools: spec.NodePools,
	}}

	findings, err := AuditOne(context.Background(), cluster, rp, spec.ID, spec.Provider, spec.Region)
	if err != nil {
		t.Fatalf("AuditOne: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %+v, want none", findings)
	}
}

func TestAuditOne_ClusterAbsent(t *testing.T) {
	spec := auditTestSpec("team-payments-prod")
	rp := seedRepoWithClusterYAML(t, spec)

	cluster := fakeCluster{state: provisioner.ClusterState{Status: provisioner.StatusAbsent}}

	findings, err := AuditOne(context.Background(), cluster, rp, spec.ID, spec.Provider, spec.Region)
	if err != nil {
		t.Fatalf("AuditOne: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want exactly one", findings)
	}
}

func TestAuditOne_AccessDrift(t *testing.T) {
	spec := auditTestSpec("team-payments-prod")
	rp := seedRepoWithClusterYAML(t, spec)

	cluster := fakeCluster{state: provisioner.ClusterState{
		Status: provisioner.StatusActive, Access: core.AccessPublic, NodePools: spec.NodePools,
	}}

	findings, err := AuditOne(context.Background(), cluster, rp, spec.ID, spec.Provider, spec.Region)
	if err != nil {
		t.Fatalf("AuditOne: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want exactly one access finding", findings)
	}
}

func TestAuditOne_NodePoolResizeDrift(t *testing.T) {
	spec := auditTestSpec("team-payments-prod")
	rp := seedRepoWithClusterYAML(t, spec)

	drifted := spec.NodePools[0]
	drifted.DesiredSize = 10 // someone manually resized it

	cluster := fakeCluster{state: provisioner.ClusterState{
		Status: provisioner.StatusActive, Access: spec.Access, NodePools: []core.NodePool{drifted},
	}}

	findings, err := AuditOne(context.Background(), cluster, rp, spec.ID, spec.Provider, spec.Region)
	if err != nil {
		t.Fatalf("AuditOne: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want exactly one node pool finding", findings)
	}
}

func TestAuditOne_MissingNodePool(t *testing.T) {
	spec := auditTestSpec("team-payments-prod")
	rp := seedRepoWithClusterYAML(t, spec)

	cluster := fakeCluster{state: provisioner.ClusterState{
		Status: provisioner.StatusActive, Access: spec.Access, NodePools: nil,
	}}

	findings, err := AuditOne(context.Background(), cluster, rp, spec.ID, spec.Provider, spec.Region)
	if err != nil {
		t.Fatalf("AuditOne: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want exactly one missing-pool finding", findings)
	}
}
