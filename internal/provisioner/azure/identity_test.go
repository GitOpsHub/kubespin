package azure

import (
	"context"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

func TestIdentityProvisioner_ProvisionForComponent_CreatesAndBinds(t *testing.T) {
	f := newFakeAzure()
	spec := testSpec()
	f.activeCluster(spec)
	p := NewIdentityProvisioner(f.clients())
	comp := provisioner.StatusReporter()

	binding, err := p.ProvisionForComponent(context.Background(), spec, comp)
	if err != nil {
		t.Fatalf("ProvisionForComponent: %v", err)
	}

	n := names{spec}
	wantClientID := "client-" + n.identity(comp.Name)
	if binding.Identifier != wantClientID {
		t.Errorf("identifier = %q, want %q", binding.Identifier, wantClientID)
	}
	if binding.Annotations["azure.workload.identity/client-id"] != wantClientID {
		t.Errorf("annotation = %q, want %q",
			binding.Annotations["azure.workload.identity/client-id"], wantClientID)
	}

	if _, ok := f.identities[n.identity(comp.Name)]; !ok {
		t.Fatal("expected the managed identity to exist")
	}

	cred, ok := f.federated[n.identity(comp.Name)+"/"+n.federatedCredential(comp.Name)]
	if !ok {
		t.Fatal("expected a federated credential to exist")
	}
	wantSubject := "system:serviceaccount:" + comp.Namespace + ":" + comp.ServiceAccount
	if deref(cred.Properties.Subject) != wantSubject {
		t.Errorf("subject = %q, want %q", deref(cred.Properties.Subject), wantSubject)
	}
	if deref(cred.Properties.Issuer) != testIssuer {
		t.Errorf("issuer = %q, want %q", deref(cred.Properties.Issuer), testIssuer)
	}
}

func TestIdentityProvisioner_ProvisionForComponent_Idempotent(t *testing.T) {
	f := newFakeAzure()
	spec := testSpec()
	f.activeCluster(spec)
	p := NewIdentityProvisioner(f.clients())
	comp := provisioner.StatusReporter()

	if _, err := p.ProvisionForComponent(context.Background(), spec, comp); err != nil {
		t.Fatalf("first ProvisionForComponent: %v", err)
	}

	if _, err := p.ProvisionForComponent(context.Background(), spec, comp); err != nil {
		t.Fatalf("second ProvisionForComponent: %v", err)
	}

	if got := countCalls(f, "CreateOrUpdateIdentity"); got != 1 {
		t.Errorf("CreateOrUpdateIdentity called %d times, want 1", got)
	}
	if got := countCalls(f, "CreateOrUpdateFederatedCredential"); got != 1 {
		t.Errorf("CreateOrUpdateFederatedCredential called %d times, want 1", got)
	}
}

func TestIdentityProvisioner_Deprovision(t *testing.T) {
	f := newFakeAzure()
	spec := testSpec()
	f.activeCluster(spec)
	p := NewIdentityProvisioner(f.clients())
	comp := provisioner.StatusReporter()

	if _, err := p.ProvisionForComponent(context.Background(), spec, comp); err != nil {
		t.Fatalf("ProvisionForComponent: %v", err)
	}
	if err := p.Deprovision(context.Background(), spec, comp); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}

	n := names{spec}
	if _, ok := f.identities[n.identity(comp.Name)]; ok {
		t.Error("expected the managed identity to be gone")
	}

	// Deprovisioning an absent identity converges rather than erroring.
	if err := p.Deprovision(context.Background(), spec, comp); err != nil {
		t.Fatalf("second Deprovision should converge: %v", err)
	}
}

func TestIdentityProvisioner_ProvisionForComponent_RequiresActiveCluster(t *testing.T) {
	f := newFakeAzure()
	p := NewIdentityProvisioner(f.clients())

	if _, err := p.ProvisionForComponent(context.Background(), testSpec(), provisioner.StatusReporter()); err == nil {
		t.Fatal("expected an error binding identity on an absent cluster")
	}
}

func countCalls(f *fakeAzure, name string) int {
	n := 0
	for _, c := range f.calls {
		if c == name {
			n++
		}
	}
	return n
}
