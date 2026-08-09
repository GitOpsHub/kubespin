package gcp

import (
	"context"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

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
