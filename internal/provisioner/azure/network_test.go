package azure

import (
	"testing"
)

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
