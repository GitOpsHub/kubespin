package gcp

import (
	"context"
	"strings"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

// An existing spec's subnets pass through unchanged — the operator owns
// that network.
func TestEnsureNetwork_PassesThroughExistingSubnets(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	p := NewNetworkProvisioner(f.clients())

	result, err := p.EnsureNetwork(context.Background(), spec)
	if err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if len(result.SubnetIDs) == 0 || result.Change.Changed {
		t.Errorf("result = %+v, want spec.Subnets passed through unchanged", result)
	}
	if f.called("InsertNetwork") || f.called("InsertSubnetwork") {
		t.Error("a network was created despite an operator-supplied subnet")
	}
}

// A clean project with no subnets supplied gets a custom-mode VPC network
// and a subnetwork in the cluster's region.
func TestEnsureNetwork_CreatesNetworkAndSubnetworkWhenSubnetsEmpty(t *testing.T) {
	f := newFakeGCP()
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
		t.Fatalf("SubnetIDs = %v, want exactly one resolved subnetwork", result.SubnetIDs)
	}
	if !strings.Contains(result.SubnetIDs[0], "team-payments-prod-subnet") {
		t.Errorf("subnetwork resource %q does not name the derived subnetwork", result.SubnetIDs[0])
	}
	if !strings.HasPrefix(result.SubnetIDs[0], "projects/"+testProject+"/regions/"+spec.Region+"/subnetworks/") {
		t.Errorf("subnetwork resource %q does not match the expected path format", result.SubnetIDs[0])
	}

	for _, want := range []string{"InsertNetwork", "InsertSubnetwork"} {
		if !f.called(want) {
			t.Errorf("%s was not called", want)
		}
	}

	n := names{project: testProject, spec: spec}
	if _, ok := f.networks[n.network()]; !ok {
		t.Error("network was not recorded as created")
	}
}

// A repeated apply must not create duplicate resources or report a change.
func TestEnsureNetwork_IsIdempotent(t *testing.T) {
	f := newFakeGCP()
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
		t.Errorf("SubnetIDs = %v, want the same subnetwork as the first call (%v)", second.SubnetIDs, first.SubnetIDs)
	}
	f.assertNoMutations(t)
}

func TestEnsureNetwork_RespectsSubnetCIDROverride(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	spec.Subnets = nil
	spec.SubnetCIDR = "172.16.0.0/20"

	if _, err := NewNetworkProvisioner(f.clients()).EnsureNetwork(t.Context(), spec); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}

	n := names{project: testProject, spec: spec}
	sub, ok := f.subnetworks[n.location()+"/"+n.subnetwork()]
	if !ok {
		t.Fatal("subnetwork was not created")
	}
	if sub.IpCidrRange != spec.SubnetCIDR {
		t.Errorf("subnetwork CIDR = %q, want %q", sub.IpCidrRange, spec.SubnetCIDR)
	}
}

// GKE nodes are always private (EnablePrivateNodes), so a Cloud Router +
// Cloud NAT is what actually gives them a path to the internet — without
// one, nothing they run can pull an image from a public registry.
func TestEnsureNetwork_CreatesCloudNATForPrivateNodes(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	spec.Subnets = nil

	if _, err := NewNetworkProvisioner(f.clients()).EnsureNetwork(t.Context(), spec); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}

	if !f.called("InsertRouter") {
		t.Error("InsertRouter was not called")
	}

	n := names{project: testProject, spec: spec}
	router, ok := f.routers[n.location()+"/"+n.router()]
	if !ok {
		t.Fatal("router was not recorded as created")
	}
	if len(router.Nats) != 1 {
		t.Fatalf("router.Nats = %v, want exactly one NAT config", router.Nats)
	}
	if router.Nats[0].SourceSubnetworkIpRangesToNat != "ALL_SUBNETWORKS_ALL_IP_RANGES" {
		t.Errorf("NAT does not cover all subnetwork ranges: %+v", router.Nats[0])
	}
}

// A repeated apply must not attempt to create the router again.
func TestEnsureNetwork_CloudNATIsIdempotent(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	spec.Subnets = nil
	p := NewNetworkProvisioner(f.clients())

	if _, err := p.EnsureNetwork(t.Context(), spec); err != nil {
		t.Fatalf("first call: %v", err)
	}

	f.calls = nil
	if _, err := p.EnsureNetwork(t.Context(), spec); err != nil {
		t.Fatalf("second call: %v", err)
	}
	f.assertNoMutations(t)
}

func (f *fakeGCP) called(name string) bool {
	for _, c := range f.calls {
		if c == name {
			return true
		}
	}
	return false
}

func TestNetworkProvisioner_AllowEgress_CreatesRule(t *testing.T) {
	f := newFakeGCP()
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

	name := "kubespin-" + spec.ID.String() + "-egress"
	fw, ok := f.firewalls[name]
	if !ok {
		t.Fatal("expected a firewall rule to have been created")
	}
	if fw.Direction != "EGRESS" {
		t.Errorf("direction = %q, want EGRESS", fw.Direction)
	}
}

func TestNetworkProvisioner_AllowEgress_Idempotent(t *testing.T) {
	f := newFakeGCP()
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

	insertCalls := 0
	for _, c := range f.calls {
		if c == "InsertFirewall" {
			insertCalls++
		}
	}
	if insertCalls != 1 {
		t.Errorf("InsertFirewall called %d times, want 1", insertCalls)
	}
}

func TestNetworkProvisioner_AllowEgress_RequiresNetwork(t *testing.T) {
	f := newFakeGCP()
	p := NewNetworkProvisioner(f.clients())

	_, err := p.AllowEgress(context.Background(), testSpec(), provisioner.EgressDestination{Host: "x"})
	if err == nil {
		t.Fatal("expected an error opening egress on an absent cluster")
	}
}

func TestDeleteNetwork_DeletesEverythingEnsureNetworkCreated(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	spec.Subnets = nil
	p := NewNetworkProvisioner(f.clients())

	if _, err := p.EnsureNetwork(t.Context(), spec); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	networkName := "kubespin-" + spec.ID.String()
	if _, ok := f.networks[networkName]; !ok {
		t.Fatal("EnsureNetwork created no network; nothing for DeleteNetwork to prove")
	}
	// A Cloud Router/NAT is only created when PublicNodes is unset — confirm
	// DeleteNetwork also reverses it in the default (private-nodes) case.
	if len(f.routers) == 0 {
		t.Fatal("EnsureNetwork created no router")
	}

	if err := p.DeleteNetwork(t.Context(), spec); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}

	if _, ok := f.networks[networkName]; ok {
		t.Error("network left behind")
	}
	if len(f.subnetworks) != 1 { // the fixture "us-central1/default" entry stays
		t.Errorf("%d subnetwork(s) left behind beyond the fixture", len(f.subnetworks)-1)
	}
	if len(f.routers) != 0 {
		t.Errorf("%d router(s) left behind", len(f.routers))
	}
}

// An operator-supplied --subnets network was never created by kubespin, so
// DeleteNetwork must never touch it — this is what protects it, since delete
// may not have --subnets re-supplied the way apply did.
func TestDeleteNetwork_NoOpWhenNetworkWasNeverCreated(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec() // carries operator-supplied Subnets

	if err := NewNetworkProvisioner(f.clients()).DeleteNetwork(t.Context(), spec); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	f.assertNoMutations(t)
}

// GKE cleans up its own auto-created firewall rules asynchronously after
// DeleteCluster's operation reports done; DeleteNetwork must ride that race
// out rather than failing the whole teardown on it.
func TestDeleteNetwork_RetriesResourceInUseOnDeleteNetwork(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	spec.Subnets = nil
	p := NewNetworkProvisioner(f.clients())
	p.retryInterval = 0

	if _, err := p.EnsureNetwork(t.Context(), spec); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	f.deleteNetworkInUseErrors = 2

	if err := p.DeleteNetwork(t.Context(), spec); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	networkName := "kubespin-" + spec.ID.String()
	if _, ok := f.networks[networkName]; ok {
		t.Error("network still present after DeleteNetwork retried past resourceInUseByAnotherResource")
	}
}
