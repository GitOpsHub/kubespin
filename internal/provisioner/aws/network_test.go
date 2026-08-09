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

func TestAllowEgress_RequiresAClusterSecurityGroup(t *testing.T) {
	f := newFakeAWS()

	_, err := NewNetworkProvisioner(f.clients()).
		AllowEgress(t.Context(), testSpec(), ingestionDestination())
	if !errors.Is(err, provisioner.ErrNotFound) {
		t.Fatalf("error = %v, want one wrapping ErrNotFound", err)
	}
}
