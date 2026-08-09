package azure

import (
	"strings"
	"testing"
)

func TestEnsureNetwork_CreatesVNetAndSubnetWhenSubnetsEmpty(t *testing.T) {
	f := newFakeAzure()
	spec := testSpec()
	spec.Subnets = nil

	result, err := NewNetworkProvisioner(f.clients()).EnsureNetwork(t.Context(), spec)
	if err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}

	if !result.Change.Changed {
		t.Error("Changed = false, want the new network reported")
	}
	if len(result.SubnetIDs) != 1 || result.SubnetIDs[0] == "" {
		t.Fatalf("SubnetIDs = %v, want exactly one resolved subnet ID", result.SubnetIDs)
	}
	if !strings.Contains(result.SubnetIDs[0], "team-payments-prod-subnet") {
		t.Errorf("subnet ID %q does not name the derived subnet", result.SubnetIDs[0])
	}

	for _, want := range []string{"EnsureResourceGroup", "CreateOrUpdateVirtualNetwork", "CreateOrUpdateSubnet"} {
		if !f.called(want) {
			t.Errorf("%s was not called", want)
		}
	}

	n := names{spec}
	if _, ok := f.resourceGroups[n.resourceGroup()]; !ok {
		t.Error("resource group was not recorded as created")
	}
}

// A repeated apply must not create duplicate resources or report a change.
func TestEnsureNetwork_IsIdempotent(t *testing.T) {
	f := newFakeAzure()
	spec := testSpec()
	spec.Subnets = nil
	p := NewNetworkProvisioner(f.clients())

	first, err := p.EnsureNetwork(t.Context(), spec)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	f.calls = nil
	second, err := p.EnsureNetwork(t.Context(), spec)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if second.Change.Changed {
		t.Errorf("Changed = true on a second call: %v", second.Change.Details)
	}
	if len(second.SubnetIDs) != 1 || second.SubnetIDs[0] != first.SubnetIDs[0] {
		t.Errorf("SubnetIDs = %v, want the same subnet as the first call (%v)", second.SubnetIDs, first.SubnetIDs)
	}
	f.assertNoMutations(t)
}

// An operator-supplied subnet is left alone — kubespin only ensures the
// resource group it and the cluster both need.
func TestEnsureNetwork_PassesThroughExistingSubnets(t *testing.T) {
	f := newFakeAzure()
	spec := testSpec() // testSpec already sets Subnets

	result, err := NewNetworkProvisioner(f.clients()).EnsureNetwork(t.Context(), spec)
	if err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}

	if len(result.SubnetIDs) != len(spec.Subnets) || result.SubnetIDs[0] != spec.Subnets[0] {
		t.Errorf("SubnetIDs = %v, want spec.Subnets unchanged", result.SubnetIDs)
	}
	if f.called("CreateOrUpdateVirtualNetwork") || f.called("CreateOrUpdateSubnet") {
		t.Error("a network was created despite an operator-supplied subnet")
	}
	if !f.called("EnsureResourceGroup") {
		t.Error("the resource group was not ensured even though the subnet was supplied")
	}
}

func TestEnsureNetwork_RespectsCIDROverrides(t *testing.T) {
	f := newFakeAzure()
	spec := testSpec()
	spec.Subnets = nil
	spec.VNetCIDR = "172.16.0.0/16"
	spec.SubnetCIDR = "172.16.1.0/24"

	if _, err := NewNetworkProvisioner(f.clients()).EnsureNetwork(t.Context(), spec); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}

	n := names{spec}
	vnet, ok := f.vnets[n.resourceGroup()+"/"+n.vnet()]
	if !ok {
		t.Fatal("virtual network was not created")
	}
	got := vnet.Properties.AddressSpace.AddressPrefixes
	if len(got) != 1 || deref(got[0]) != spec.VNetCIDR {
		t.Errorf("VNet address space = %v, want [%s]", got, spec.VNetCIDR)
	}

	subnet, ok := f.subnets[n.resourceGroup()+"/"+n.vnet()+"/"+n.subnet()]
	if !ok {
		t.Fatal("subnet was not created")
	}
	if deref(subnet.Properties.AddressPrefix) != spec.SubnetCIDR {
		t.Errorf("subnet prefix = %q, want %q", deref(subnet.Properties.AddressPrefix), spec.SubnetCIDR)
	}
}

func (f *fakeAzure) called(name string) bool {
	for _, c := range f.calls {
		if c == name {
			return true
		}
	}
	return false
}
