package gcp

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/api/googleapi"
	iam "google.golang.org/api/iam/v1"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

// workloadIdentityUserRole is the IAM role that lets a Kubernetes service
// account impersonate a Google service account. It is the entire binding:
// Workload Identity needs no separate OIDC provider registration the way IRSA
// does, because GKE's workload pool is the trust root for every cluster in
// the project.
const workloadIdentityUserRole = "roles/iam.workloadIdentityUser"

// IdentityProvisioner binds in-cluster service accounts to Google service
// accounts via Workload Identity.
type IdentityProvisioner struct {
	c *Clients
}

// NewIdentityProvisioner builds a Workload Identity provisioner.
func NewIdentityProvisioner(c *Clients) *IdentityProvisioner { return &IdentityProvisioner{c: c} }

// Provider identifies this implementation's cloud.
func (p *IdentityProvisioner) Provider() core.Provider { return core.ProviderGCP }

// ProvisionForComponent creates the component's Google service account if
// needed and binds it to the component's Kubernetes service account.
//
// The service account carries no permission policy. The identity exists so
// the component can *prove* which cluster it is when it pushes status;
// granting it GCP access would be a separate, deliberate decision.
func (p *IdentityProvisioner) ProvisionForComponent(
	ctx context.Context, spec core.ClusterSpec, comp provisioner.Component,
) (provisioner.Binding, error) {
	n := names{project: p.c.project, spec: spec}

	cluster := NewClusterProvisioner(p.c)
	state, err := cluster.Describe(ctx, spec)
	if err != nil {
		return provisioner.Binding{}, err
	}
	if state.Status != provisioner.StatusActive {
		// GKE's Workload Identity pool exists whether or not the cluster is up,
		// but binding identity before the cluster is usable is not meaningful
		// work, so this is kept a separate phase like AWS's IRSA binding.
		return provisioner.Binding{}, fmt.Errorf(
			"%w: %s is %s; workload identity needs an active cluster",
			provisioner.ErrNotFound, spec.ID, state.Status)
	}

	if err := p.ensureServiceAccount(ctx, n, comp); err != nil {
		return provisioner.Binding{}, err
	}

	if err := p.bindWorkloadIdentity(ctx, n, comp); err != nil {
		return provisioner.Binding{}, err
	}

	email := n.serviceAccountEmail(comp.Name)
	return provisioner.Binding{
		Identifier: email,
		// The caller applies this blind: each cloud uses a different key, and
		// the caller should not have to know which cloud it is on.
		Annotations: map[string]string{"iam.gke.io/gcp-service-account": email},
	}, nil
}

func (p *IdentityProvisioner) ensureServiceAccount(ctx context.Context, n names, comp provisioner.Component) error {
	resource := n.serviceAccountResource(comp.Name)

	if _, err := p.c.svcAccts.Get(ctx, resource); err == nil {
		return nil
	} else if code(err) != 404 {
		return fmt.Errorf("getting service account %s: %w", n.serviceAccountEmail(comp.Name), err)
	}

	_, err := p.c.svcAccts.Create(ctx, "projects/"+n.project, &iam.CreateServiceAccountRequest{
		AccountId: n.serviceAccountID(comp.Name),
		ServiceAccount: &iam.ServiceAccount{
			DisplayName: fmt.Sprintf("kubespin workload identity for %s", comp.Name),
			Description: fmt.Sprintf("kubespin-managed identity for %s in cluster %s", comp.Name, n.spec.ID),
		},
	})
	if err != nil {
		if code(err) == 409 {
			// Another run got there first; that is convergence, not failure.
			return nil
		}
		return fmt.Errorf("creating service account %s: %w", n.serviceAccountEmail(comp.Name), err)
	}
	return nil
}

// bindWorkloadIdentity scopes the service account to exactly one Kubernetes
// service account in one namespace of one cluster, the same scoping IRSA's
// trust policy gives on AWS.
//
// The policy is rewritten in place rather than replaced wholesale: other
// bindings unrelated to Workload Identity (if an operator added any by hand)
// must survive an `apply`.
func (p *IdentityProvisioner) bindWorkloadIdentity(
	ctx context.Context, n names, comp provisioner.Component,
) error {
	resource := n.serviceAccountResource(comp.Name)
	member := fmt.Sprintf("serviceAccount:%s.svc.id.goog[%s/%s]",
		n.project, comp.Namespace, comp.ServiceAccount)

	policy, err := p.c.svcAccts.GetIamPolicy(ctx, resource)
	if err != nil {
		return fmt.Errorf("reading IAM policy for %s: %w", n.serviceAccountEmail(comp.Name), err)
	}

	binding := findBinding(policy, workloadIdentityUserRole)
	if binding != nil && containsMember(binding.Members, member) {
		return nil
	}

	if binding == nil {
		policy.Bindings = append(policy.Bindings, &iam.Binding{
			Role:    workloadIdentityUserRole,
			Members: []string{member},
		})
	} else {
		binding.Members = append(binding.Members, member)
	}

	if _, err := p.c.svcAccts.SetIamPolicy(ctx, resource, &iam.SetIamPolicyRequest{Policy: policy}); err != nil {
		return fmt.Errorf("binding workload identity for %s: %w", n.serviceAccountEmail(comp.Name), err)
	}
	return nil
}

func findBinding(policy *iam.Policy, role string) *iam.Binding {
	for _, b := range policy.Bindings {
		if b.Role == role {
			return b
		}
	}
	return nil
}

func containsMember(members []string, want string) bool {
	for _, m := range members {
		if m == want {
			return true
		}
	}
	return false
}

// Deprovision removes the component's service account, which also removes its
// IAM policy bindings.
//
// Deleting an absent service account is a no-op, so a retried teardown
// converges rather than failing.
func (p *IdentityProvisioner) Deprovision(
	ctx context.Context, spec core.ClusterSpec, comp provisioner.Component,
) error {
	n := names{project: p.c.project, spec: spec}

	if err := p.c.svcAccts.Delete(ctx, n.serviceAccountResource(comp.Name)); err != nil {
		if code(err) == 404 {
			return nil
		}
		return fmt.Errorf("deleting service account %s: %w", n.serviceAccountEmail(comp.Name), err)
	}
	return nil
}

// code extracts the HTTP status code from a googleapi error, or 0 if err is
// not one. REST-based GCP clients (IAM, Compute) report errors this way,
// unlike the gRPC-based GKE client which uses grpc/status codes instead.
func code(err error) int {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return gerr.Code
	}
	return 0
}
