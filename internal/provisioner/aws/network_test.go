package aws

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// An existing spec's subnets pass through unchanged — the operator owns
// that network.
func TestEnsureNetwork_PassesThroughExistingSubnets(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()

	result, err := NewNetworkProvisioner(f.clients()).EnsureNetwork(t.Context(), spec)
	if err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if len(result.SubnetIDs) != 2 || result.Change.Changed {
		t.Errorf("result = %+v, want spec.Subnets passed through unchanged", result)
	}
	f.assertNoMutations(t)
}

// A clean account with no subnets supplied gets a VPC, two subnets across
// two AZs, an Internet Gateway, and a public route table.
func TestEnsureNetwork_CreatesVPCAndSubnetsWhenSubnetsEmpty(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	spec.Subnets = nil

	result, err := NewNetworkProvisioner(f.clients()).EnsureNetwork(t.Context(), spec)
	if err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}

	if !result.Change.Changed {
		t.Error("Changed = false, want the new network reported")
	}
	if len(result.SubnetIDs) != 2 {
		t.Fatalf("SubnetIDs = %v, want exactly two resolved subnet IDs", result.SubnetIDs)
	}
	if result.SubnetIDs[0] == result.SubnetIDs[1] {
		t.Errorf("both subnets resolved to the same ID: %v", result.SubnetIDs)
	}

	for _, want := range []string{
		"CreateVpc", "ModifyVpcAttribute", "CreateSubnet",
		"CreateInternetGateway", "AttachInternetGateway",
		"CreateRouteTable", "CreateRoute", "AssociateRouteTable",
	} {
		if !f.called(want) {
			t.Errorf("%s was not called", want)
		}
	}

	if len(f.vpcs) != 1 {
		t.Errorf("vpcs created = %d, want 1", len(f.vpcs))
	}
	if len(f.subnets) != 2 {
		t.Errorf("subnets created = %d, want 2", len(f.subnets))
	}
}

// A repeated apply must not create duplicate resources or report a change.
func TestEnsureNetwork_IsIdempotent(t *testing.T) {
	f := newFakeAWS()
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
	if len(second.SubnetIDs) != 2 || second.SubnetIDs[0] != first.SubnetIDs[0] || second.SubnetIDs[1] != first.SubnetIDs[1] {
		t.Errorf("SubnetIDs = %v, want the same subnets as the first call (%v)", second.SubnetIDs, first.SubnetIDs)
	}
	if len(f.vpcs) != 1 || len(f.subnets) != 2 {
		t.Errorf("resources duplicated on second call: %d vpcs, %d subnets", len(f.vpcs), len(f.subnets))
	}
	f.assertNoMutations(t)
}

func TestEnsureNetwork_RespectsVPCCIDROverride(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	spec.Subnets = nil
	spec.VPCCIDR = "172.16.0.0/16"

	if _, err := NewNetworkProvisioner(f.clients()).EnsureNetwork(t.Context(), spec); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}

	var found bool
	for _, v := range f.vpcs {
		if aws.ToString(v.CidrBlock) == spec.VPCCIDR {
			found = true
		}
	}
	if !found {
		t.Errorf("no VPC created with CIDR %s", spec.VPCCIDR)
	}
}

func TestDeleteNetwork_DeletesEverythingEnsureNetworkCreated(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	spec.Subnets = nil
	p := NewNetworkProvisioner(f.clients())

	if _, err := p.EnsureNetwork(t.Context(), spec); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if len(f.vpcs) == 0 {
		t.Fatal("EnsureNetwork created no VPC; nothing for DeleteNetwork to prove")
	}

	if err := p.DeleteNetwork(t.Context(), spec); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}

	if len(f.vpcs) != 0 {
		t.Errorf("%d VPC(s) left behind", len(f.vpcs))
	}
	if len(f.subnets) != 0 {
		t.Errorf("%d subnet(s) left behind", len(f.subnets))
	}
	if len(f.igws) != 0 {
		t.Errorf("%d internet gateway(s) left behind", len(f.igws))
	}
	if len(f.routeTables) != 0 {
		t.Errorf("%d route table(s) left behind", len(f.routeTables))
	}
}

// An operator-supplied --subnets network was never created by kubespin, so
// DeleteNetwork must never touch it — this is what protects it, since delete
// may not have --subnets re-supplied the way apply did.
func TestDeleteNetwork_NoOpWhenNetworkWasNeverCreated(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec() // carries operator-supplied Subnets

	if err := NewNetworkProvisioner(f.clients()).DeleteNetwork(t.Context(), spec); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	f.assertNoMutations(t)
}

// The ENI a just-deleted load balancer owned can take tens of seconds to
// detach; DeleteNetwork must ride that out rather than failing the whole
// teardown on it.
func TestDeleteNetwork_RetriesDependencyViolationOnDeleteVpc(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	spec.Subnets = nil
	p := NewNetworkProvisioner(f.clients())
	p.retryInterval = 0

	if _, err := p.EnsureNetwork(t.Context(), spec); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	f.deleteVPCDependencyErrors = 2

	if err := p.DeleteNetwork(t.Context(), spec); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	if len(f.vpcs) != 0 {
		t.Error("VPC still present after DeleteNetwork retried past DependencyViolation")
	}
}

// A run interrupted partway through creating the network leaves resources
// that exist, found by Name tag, but were never configured. A resumed apply
// must finish configuring them instead of adopting them as done.
func TestEnsureNetwork_ConvergesAPartiallyCreatedNetwork(t *testing.T) {
	tests := map[string]struct {
		undo  func(f *fakeAWS)
		check func(t *testing.T, f *fakeAWS)
	}{
		"VPC DNS never enabled": {
			undo: func(f *fakeAWS) {
				for _, dns := range f.vpcDNS {
					dns.hostnames = false
				}
			},
			check: func(t *testing.T, f *fakeAWS) {
				for id, dns := range f.vpcDNS {
					if !dns.support || !dns.hostnames {
						t.Errorf("VPC %s DNS = %+v, want both enabled", id, *dns)
					}
				}
			},
		},
		"subnet public IP never enabled": {
			undo: func(f *fakeAWS) {
				for _, s := range f.subnets {
					s.MapPublicIpOnLaunch = nil
				}
			},
			check: func(t *testing.T, f *fakeAWS) {
				for id, s := range f.subnets {
					if !aws.ToBool(s.MapPublicIpOnLaunch) {
						t.Errorf("subnet %s does not auto-assign public IPs", id)
					}
				}
			},
		},
		"internet gateway never attached": {
			undo: func(f *fakeAWS) {
				for _, igw := range f.igws {
					igw.Attachments = nil
				}
			},
			check: func(t *testing.T, f *fakeAWS) {
				for id, igw := range f.igws {
					if len(igw.Attachments) != 1 {
						t.Errorf("internet gateway %s attachments = %v, want one", id, igw.Attachments)
					}
				}
			},
		},
		"route table has no route or associations": {
			undo: func(f *fakeAWS) {
				for _, rt := range f.routeTables {
					rt.Routes = nil
					rt.Associations = nil
				}
			},
			check: func(t *testing.T, f *fakeAWS) {
				for id, rt := range f.routeTables {
					if !hasDefaultRoute(*rt) {
						t.Errorf("route table %s has no default route", id)
					}
					for subnetID := range f.subnets {
						if !isAssociated(*rt, subnetID) {
							t.Errorf("route table %s not associated with subnet %s", id, subnetID)
						}
					}
				}
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFakeAWS()
			spec := testSpec()
			spec.Subnets = nil
			p := NewNetworkProvisioner(f.clients())

			if _, err := p.EnsureNetwork(t.Context(), spec); err != nil {
				t.Fatalf("first EnsureNetwork: %v", err)
			}
			tc.undo(f)

			result, err := p.EnsureNetwork(t.Context(), spec)
			if err != nil {
				t.Fatalf("resumed EnsureNetwork: %v", err)
			}
			if !result.Change.Changed {
				t.Error("Changed = false, want the repair reported")
			}
			tc.check(t, f)

			// And once repaired, it stays converged with no writes at all.
			f.calls = nil
			again, err := p.EnsureNetwork(t.Context(), spec)
			if err != nil {
				t.Fatalf("third EnsureNetwork: %v", err)
			}
			if again.Change.Changed {
				t.Errorf("converged network still reports changes: %v", again.Change.Details)
			}
			f.assertNoMutations(t)
		})
	}
}

// EKS rejects control plane subnets in a Local Zone, and a Local Zone's name
// sorts ahead of the region's own zones, so it must be filtered out.
func TestEnsureNetwork_NeverPicksALocalZone(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	spec.Subnets = nil

	if _, err := NewNetworkProvisioner(f.clients()).EnsureNetwork(t.Context(), spec); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	for id, s := range f.subnets {
		if az := aws.ToString(s.AvailabilityZone); az != "us-east-1a" && az != "us-east-1b" {
			t.Errorf("subnet %s placed in %s, want us-east-1a or us-east-1b", id, az)
		}
	}
}
