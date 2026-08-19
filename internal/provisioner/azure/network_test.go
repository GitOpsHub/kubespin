package azure

import (
	"context"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

func TestNetworkProvisioner_AllowEgress_CreatesRule(t *testing.T) {
	f := newFakeAzure()
	spec := testSpec()
	f.activeCluster(spec)
	p := NewNetworkProvisioner(f.clients())

	dest := provisioner.EgressDestination{Host: "ingest.kubespin.example.com", Port: 443}
	change, err := p.AllowEgress(context.Background(), spec, dest)
	if err != nil {
		t.Fatalf("AllowEgress: %v", err)
	}
	if !change.Changed {
		t.Fatal("expected the first call to report a change")
	}

	n := names{spec}
	rule, ok := f.rules["aks-agentpool-nsg/"+n.securityRule()]
	if !ok {
		t.Fatal("expected a security rule to have been created")
	}
	if deref((*string)(rule.Properties.Direction)) != "Outbound" {
		t.Errorf("direction = %q, want Outbound", deref((*string)(rule.Properties.Direction)))
	}
}

func TestNetworkProvisioner_AllowEgress_Idempotent(t *testing.T) {
	f := newFakeAzure()
	spec := testSpec()
	f.activeCluster(spec)
	p := NewNetworkProvisioner(f.clients())

	dest := provisioner.EgressDestination{Host: "ingest.kubespin.example.com", Port: 443}
	if _, err := p.AllowEgress(context.Background(), spec, dest); err != nil {
		t.Fatalf("first AllowEgress: %v", err)
	}

	change, err := p.AllowEgress(context.Background(), spec, dest)
	if err != nil {
		t.Fatalf("second AllowEgress: %v", err)
	}
	if change.Changed {
		t.Error("expected the second call to report no change")
	}

	if got := countCalls(f, "CreateOrUpdateSecurityRule"); got != 1 {
		t.Errorf("CreateOrUpdateSecurityRule called %d times, want 1", got)
	}
}

func TestNetworkProvisioner_AllowEgress_RequiresNetwork(t *testing.T) {
	f := newFakeAzure()
	p := NewNetworkProvisioner(f.clients())

	_, err := p.AllowEgress(context.Background(), testSpec(), provisioner.EgressDestination{Host: "x"})
	if err == nil {
		t.Fatal("expected an error opening egress on an absent cluster")
	}
}

// Unlike AWS/GCP, EnsureNetwork always creates the resource group — even
// when --subnets points at an operator-owned VNet elsewhere — because it also
// holds the AKS cluster resource itself. DeleteNetwork must reverse that
// unconditionally too.
func TestDeleteNetwork_DeletesTheResourceGroupEnsureNetworkCreated(t *testing.T) {
	f := newFakeAzure()
	spec := testSpec()
	p := NewNetworkProvisioner(f.clients())

	if _, err := p.EnsureNetwork(t.Context(), spec); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	n := names{spec}
	if _, ok := f.resourceGroups[n.resourceGroup()]; !ok {
		t.Fatal("EnsureNetwork created no resource group; nothing for DeleteNetwork to prove")
	}

	if err := p.DeleteNetwork(t.Context(), spec); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	if _, ok := f.resourceGroups[n.resourceGroup()]; ok {
		t.Error("resource group left behind")
	}
}

func TestDeleteNetwork_NoOpWhenResourceGroupWasNeverCreated(t *testing.T) {
	f := newFakeAzure()

	if err := NewNetworkProvisioner(f.clients()).DeleteNetwork(t.Context(), testSpec()); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	f.assertNoMutations(t)
}
