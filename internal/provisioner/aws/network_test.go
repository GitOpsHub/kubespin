package aws

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

func ingestionDestination() provisioner.EgressDestination {
	return provisioner.EgressDestination{
		Host:        "abc.execute-api.us-east-1.amazonaws.com",
		Port:        443,
		Description: "kubespin fleet-status-reporter egress",
	}
}

func TestAllowEgress_CreatesTheRule(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	f.activeCluster(spec)

	change, err := NewNetworkProvisioner(f.clients()).
		AllowEgress(t.Context(), spec, ingestionDestination())
	if err != nil {
		t.Fatalf("AllowEgress: %v", err)
	}

	if !change.Changed {
		t.Error("Changed = false, want the new rule reported")
	}
	if !f.called("AuthorizeSecurityGroupEgress") {
		t.Error("no egress rule was authorised")
	}
}

// A repeated apply must not accumulate duplicate rules.
func TestAllowEgress_IsIdempotent(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	f.activeCluster(spec)
	p := NewNetworkProvisioner(f.clients())

	if _, err := p.AllowEgress(t.Context(), spec, ingestionDestination()); err != nil {
		t.Fatalf("first call: %v", err)
	}

	f.calls = nil
	change, err := p.AllowEgress(t.Context(), spec, ingestionDestination())
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if change.Changed {
		t.Errorf("Changed = true on a second call: %v", change.Details)
	}
	f.assertNoMutations(t)
}

// An existing allow-all egress rule already covers the destination; adding a
// narrower duplicate alongside it would be noise.
func TestAllowEgress_RecognisesAnAllowAllRule(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	f.activeCluster(spec)
	f.sgRules = []ec2types.SecurityGroupRule{{
		IsEgress:   aws.Bool(true),
		IpProtocol: aws.String("-1"),
		CidrIpv4:   aws.String("0.0.0.0/0"),
	}}

	change, err := NewNetworkProvisioner(f.clients()).
		AllowEgress(t.Context(), spec, ingestionDestination())
	if err != nil {
		t.Fatalf("AllowEgress: %v", err)
	}

	if change.Changed {
		t.Error("a rule was added despite an existing allow-all egress rule")
	}
	f.assertNoMutations(t)
}

// An inbound rule on the same port must not be mistaken for egress.
func TestAllowEgress_IgnoresIngressRules(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	f.activeCluster(spec)
	f.sgRules = []ec2types.SecurityGroupRule{{
		IsEgress:   aws.Bool(false),
		IpProtocol: aws.String("tcp"),
		FromPort:   aws.Int32(443),
		ToPort:     aws.Int32(443),
		CidrIpv4:   aws.String("0.0.0.0/0"),
	}}

	change, err := NewNetworkProvisioner(f.clients()).
		AllowEgress(t.Context(), spec, ingestionDestination())
	if err != nil {
		t.Fatalf("AllowEgress: %v", err)
	}
	if !change.Changed {
		t.Error("an ingress rule was mistaken for the egress rule")
	}
}

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

func TestAllowEgress_RequiresAClusterSecurityGroup(t *testing.T) {
	f := newFakeAWS()

	_, err := NewNetworkProvisioner(f.clients()).
		AllowEgress(t.Context(), testSpec(), ingestionDestination())
	if !errors.Is(err, provisioner.ErrNotFound) {
		t.Fatalf("error = %v, want one wrapping ErrNotFound", err)
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
