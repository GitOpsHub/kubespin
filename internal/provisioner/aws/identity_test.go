package aws

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

func TestProvisionForComponent_RegistersOIDCAndCreatesTheRole(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	f.activeCluster(spec)

	binding, err := NewIdentityProvisioner(f.clients()).
		ProvisionForComponent(t.Context(), spec, provisioner.StatusReporter())
	if err != nil {
		t.Fatalf("ProvisionForComponent: %v", err)
	}

	if !f.called("CreateOpenIDConnectProvider") {
		t.Error("the cluster's OIDC issuer was never registered with IAM")
	}
	if !f.called("CreateRole") {
		t.Error("no IRSA role was created")
	}

	wantRole := "kubespin-team-payments-prod-fleet-status-reporter"
	if !strings.HasSuffix(binding.Identifier, wantRole) {
		t.Errorf("Identifier = %q, want a role named %s", binding.Identifier, wantRole)
	}
	if got := binding.Annotations["eks.amazonaws.com/role-arn"]; got != binding.Identifier {
		t.Errorf("annotation = %q, want the role ARN %q", got, binding.Identifier)
	}
}

// The trust policy is the only thing standing between this role and every other
// service account in the cluster, so its conditions are asserted exactly.
func TestProvisionForComponent_TrustPolicyIsScoped(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	f.activeCluster(spec)
	comp := provisioner.StatusReporter()

	if _, err := NewIdentityProvisioner(f.clients()).
		ProvisionForComponent(t.Context(), spec, comp); err != nil {
		t.Fatalf("ProvisionForComponent: %v", err)
	}

	doc := f.trustPolicy(t, names{spec}.irsaRole(comp.Name))
	statements, ok := doc["Statement"].([]any)
	if !ok || len(statements) != 1 {
		t.Fatalf("Statement = %v, want exactly one", doc["Statement"])
	}

	statement, ok := statements[0].(map[string]any)
	if !ok {
		t.Fatalf("statement is not an object: %v", statements[0])
	}
	if statement["Action"] != "sts:AssumeRoleWithWebIdentity" {
		t.Errorf("Action = %v, want sts:AssumeRoleWithWebIdentity", statement["Action"])
	}

	conditions, ok := statement["Condition"].(map[string]any)
	if !ok {
		t.Fatalf("no Condition block: the role would be assumable by any identity in the cluster")
	}
	equals, ok := conditions["StringEquals"].(map[string]any)
	if !ok {
		t.Fatalf("no StringEquals conditions: %v", conditions)
	}

	host := strings.TrimPrefix(testIssuer, "https://")

	// Without sub, any service account in the cluster could assume the role.
	wantSub := fmt.Sprintf("system:serviceaccount:%s:%s", comp.Namespace, comp.ServiceAccount)
	if got := equals[host+":sub"]; got != wantSub {
		t.Errorf("sub condition = %v, want %q", got, wantSub)
	}

	// Without aud, a token minted for a different audience would be accepted.
	if got := equals[host+":aud"]; got != eksOIDCClientIDAudience {
		t.Errorf("aud condition = %v, want %q", got, eksOIDCClientIDAudience)
	}
}

func TestProvisionForComponent_ReusesAnExistingOIDCProvider(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	f.activeCluster(spec)
	p := NewIdentityProvisioner(f.clients())
	comp := provisioner.StatusReporter()

	if _, err := p.ProvisionForComponent(t.Context(), spec, comp); err != nil {
		t.Fatalf("first call: %v", err)
	}

	f.calls = nil
	if _, err := p.ProvisionForComponent(t.Context(), spec, comp); err != nil {
		t.Fatalf("second call: %v", err)
	}

	if f.called("CreateOpenIDConnectProvider") {
		t.Error("the OIDC provider was registered twice")
	}
	if f.called("CreateRole") {
		t.Error("the IRSA role was created twice")
	}
	// The trust policy is still rewritten: drift in it is a privilege
	// escalation, so it is corrected rather than compared.
	if !f.called("UpdateAssumeRolePolicy") {
		t.Error("the trust policy was not reasserted on an existing role")
	}
}

// The issuer only exists once the control plane is up, which is why identity
// binding is its own phase rather than part of creation.
func TestProvisionForComponent_RequiresAnActiveCluster(t *testing.T) {
	f := newFakeAWS()

	_, err := NewIdentityProvisioner(f.clients()).
		ProvisionForComponent(t.Context(), testSpec(), provisioner.StatusReporter())
	if !errors.Is(err, provisioner.ErrNotFound) {
		t.Fatalf("error = %v, want one wrapping ErrNotFound", err)
	}
	f.assertNoMutations(t)
}

func TestDeprovision(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	f.activeCluster(spec)
	p := NewIdentityProvisioner(f.clients())
	comp := provisioner.StatusReporter()

	if _, err := p.ProvisionForComponent(t.Context(), spec, comp); err != nil {
		t.Fatalf("ProvisionForComponent: %v", err)
	}

	role := names{spec}.irsaRole(comp.Name)
	f.attached[role] = []string{"arn:aws:iam::aws:policy/SomePolicy"}
	f.calls = nil

	if err := p.Deprovision(t.Context(), spec, comp); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}

	// IAM refuses to delete a role with policies still attached, so an orphaned
	// role would survive teardown if the detach were skipped.
	if !f.called("DetachRolePolicy") {
		t.Error("attached policies were not detached before deletion")
	}
	if _, exists := f.roles[role]; exists {
		t.Error("the IRSA role still exists after Deprovision")
	}

	// The OIDC provider belongs to the cluster, not this component.
	if len(f.oidc) == 0 {
		t.Error("the cluster's OIDC provider was removed with the component's role")
	}
}

func TestDeprovision_IsIdempotent(t *testing.T) {
	f := newFakeAWS()

	if err := NewIdentityProvisioner(f.clients()).
		Deprovision(t.Context(), testSpec(), provisioner.StatusReporter()); err != nil {
		t.Fatalf("Deprovision on an absent role: %v", err)
	}
}
