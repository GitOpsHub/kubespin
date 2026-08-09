package gcp

import (
	"context"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

func TestIdentityProvisioner_ProvisionForComponent_CreatesAndBinds(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	f.activeCluster(spec)
	p := NewIdentityProvisioner(f.clients())
	comp := provisioner.StatusReporter()

	binding, err := p.ProvisionForComponent(context.Background(), spec, comp)
	if err != nil {
		t.Fatalf("ProvisionForComponent: %v", err)
	}

	n := names{project: testProject, spec: spec}
	wantEmail := n.serviceAccountEmail(comp.Name)
	if binding.Identifier != wantEmail {
		t.Errorf("identifier = %q, want %q", binding.Identifier, wantEmail)
	}
	if binding.Annotations["iam.gke.io/gcp-service-account"] != wantEmail {
		t.Errorf("annotation = %q, want %q", binding.Annotations["iam.gke.io/gcp-service-account"], wantEmail)
	}

	resource := n.serviceAccountResource(comp.Name)
	if _, ok := f.svcAccts[resource]; !ok {
		t.Fatal("expected the service account to exist")
	}

	policy := f.policies[resource]
	binding2 := findBinding(policy, workloadIdentityUserRole)
	if binding2 == nil {
		t.Fatal("expected a workloadIdentityUser binding")
	}
	wantMember := "serviceAccount:" + testProject + ".svc.id.goog[" + comp.Namespace + "/" + comp.ServiceAccount + "]"
	if !containsMember(binding2.Members, wantMember) {
		t.Errorf("members = %v, want to contain %q", binding2.Members, wantMember)
	}
}

func TestIdentityProvisioner_ProvisionForComponent_Idempotent(t *testing.T) {
	f := newFakeGCP()
	spec := testSpec()
	f.activeCluster(spec)
	p := NewIdentityProvisioner(f.clients())
	comp := provisioner.StatusReporter()

	if _, err := p.ProvisionForComponent(context.Background(), spec, comp); err != nil {
		t.Fatalf("first ProvisionForComponent: %v", err)
	}

	setIamCalls := countCalls(f, "SetIamPolicy")

	if _, err := p.ProvisionForComponent(context.Background(), spec, comp); err != nil {
		t.Fatalf("second ProvisionForComponent: %v", err)
	}

	if got := countCalls(f, "CreateServiceAccount"); got != 1 {
		t.Errorf("CreateServiceAccount called %d times, want 1", got)
	}
	if got := countCalls(f, "SetIamPolicy"); got != setIamCalls {
		t.Errorf("SetIamPolicy called again on a repeat run: %d -> %d", setIamCalls, got)
	}
}

func TestIdentityProvisioner_Deprovision(t *testing.T) {
	f := newFakeGCP()
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

	n := names{project: testProject, spec: spec}
	if _, ok := f.svcAccts[n.serviceAccountResource(comp.Name)]; ok {
		t.Error("expected the service account to be gone")
	}

	// Deprovisioning an absent identity converges rather than erroring.
	if err := p.Deprovision(context.Background(), spec, comp); err != nil {
		t.Fatalf("second Deprovision should converge: %v", err)
	}
}

func TestIdentityProvisioner_ProvisionForComponent_RequiresActiveCluster(t *testing.T) {
	f := newFakeGCP()
	p := NewIdentityProvisioner(f.clients())

	if _, err := p.ProvisionForComponent(context.Background(), testSpec(), provisioner.StatusReporter()); err == nil {
		t.Fatal("expected an error binding identity on an absent cluster")
	}
}

func countCalls(f *fakeGCP, name string) int {
	n := 0
	for _, c := range f.calls {
		if c == name {
			n++
		}
	}
	return n
}
