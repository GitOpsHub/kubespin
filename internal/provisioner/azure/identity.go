package azure

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/msi/armmsi"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

// workloadIdentityAudience is the fixed audience Azure AD Workload Identity
// federated credentials are issued for. It is not cluster-specific: what
// scopes a credential to one cluster is the issuer URL and subject, not the
// audience.
const workloadIdentityAudience = "api://AzureADTokenExchange"

// IdentityProvisioner binds in-cluster service accounts to Azure managed
// identities via Workload Identity federated credentials.
type IdentityProvisioner struct {
	c *Clients
}

// NewIdentityProvisioner builds a Workload Identity provisioner.
func NewIdentityProvisioner(c *Clients) *IdentityProvisioner { return &IdentityProvisioner{c: c} }

// Provider identifies this implementation's cloud.
func (p *IdentityProvisioner) Provider() core.Provider { return core.ProviderAzure }

// ProvisionForComponent creates the component's user-assigned managed
// identity if needed and federates it to the component's Kubernetes service
// account via the cluster's OIDC issuer.
//
// The identity carries no role assignment. It exists so the component can
// *prove* which cluster it is when it pushes status; granting it Azure access
// would be a separate, deliberate decision.
func (p *IdentityProvisioner) ProvisionForComponent(
	ctx context.Context, spec core.ClusterSpec, comp provisioner.Component,
) (provisioner.Binding, error) {
	n := names{spec}

	cluster := NewClusterProvisioner(p.c)
	state, err := cluster.Describe(ctx, spec)
	if err != nil {
		return provisioner.Binding{}, err
	}
	if state.Status != provisioner.StatusActive {
		// The issuer only exists once the control plane is up, which is why
		// identity binding is its own phase rather than part of creation.
		return provisioner.Binding{}, fmt.Errorf(
			"%w: %s is %s; workload identity needs an active cluster",
			provisioner.ErrNotFound, spec.ID, state.Status)
	}
	if state.OIDCIssuer == "" {
		return provisioner.Binding{}, fmt.Errorf("cluster %s reports no OIDC issuer", spec.ID)
	}

	clientID, err := p.ensureIdentity(ctx, n, spec, comp)
	if err != nil {
		return provisioner.Binding{}, err
	}

	if err := p.ensureFederatedCredential(ctx, n, comp, state.OIDCIssuer); err != nil {
		return provisioner.Binding{}, err
	}

	return provisioner.Binding{
		Identifier: clientID,
		// The caller applies this blind: each cloud uses a different key, and
		// the caller should not have to know which cloud it is on.
		Annotations: map[string]string{"azure.workload.identity/client-id": clientID},
	}, nil
}

func (p *IdentityProvisioner) ensureIdentity(
	ctx context.Context, n names, spec core.ClusterSpec, comp provisioner.Component,
) (string, error) {
	name := n.identity(comp.Name)

	if existing, err := p.c.identity.GetIdentity(ctx, n.resourceGroup(), name); err == nil {
		return deref(existing.Properties.ClientID), nil
	} else if code(err) != 404 {
		return "", fmt.Errorf("getting managed identity %s: %w", name, err)
	}

	created, err := p.c.identity.CreateOrUpdateIdentity(ctx, n.resourceGroup(), name, armmsi.Identity{
		Location: ptr(spec.Region),
		Tags:     tags(spec),
	})
	if err != nil {
		return "", fmt.Errorf("creating managed identity %s: %w", name, err)
	}
	p.c.logger.Info("created managed identity", "identity", name, "component", comp.Name, "cluster", spec.ID)
	return deref(created.Properties.ClientID), nil
}

// ensureFederatedCredential scopes the identity to exactly one Kubernetes
// service account in one namespace of one cluster, the same scoping IRSA's
// trust policy gives on AWS and Workload Identity's binding gives on GCP.
func (p *IdentityProvisioner) ensureFederatedCredential(
	ctx context.Context, n names, comp provisioner.Component, issuer string,
) error {
	name := n.federatedCredential(comp.Name)
	subject := fmt.Sprintf("system:serviceaccount:%s:%s", comp.Namespace, comp.ServiceAccount)

	existing, err := p.c.identity.GetFederatedCredential(ctx, n.resourceGroup(), n.identity(comp.Name), name)
	if err == nil {
		if existing.Properties != nil &&
			deref(existing.Properties.Issuer) == issuer &&
			deref(existing.Properties.Subject) == subject {
			return nil
		}
		// Fall through to CreateOrUpdate: the credential drifted (a changed
		// issuer or subject), and Azure's API treats this call as an upsert.
	} else if code(err) != 404 {
		return fmt.Errorf("getting federated credential %s: %w", name, err)
	}

	cred := armmsi.FederatedIdentityCredential{
		Properties: &armmsi.FederatedIdentityCredentialProperties{
			Issuer:    ptr(issuer),
			Subject:   ptr(subject),
			Audiences: ptrSlice([]string{workloadIdentityAudience}),
		},
	}
	if err := p.c.identity.CreateOrUpdateFederatedCredential(
		ctx, n.resourceGroup(), n.identity(comp.Name), name, cred,
	); err != nil {
		return fmt.Errorf("binding workload identity for %s: %w", comp.Name, err)
	}
	p.c.logger.Info("bound workload identity", "identity", n.identity(comp.Name), "component", comp.Name, "issuer", issuer)
	return nil
}

// Deprovision removes the component's federated credential and managed
// identity.
//
// Deleting an absent identity is a no-op, so a retried teardown converges
// rather than failing.
func (p *IdentityProvisioner) Deprovision(
	ctx context.Context, spec core.ClusterSpec, comp provisioner.Component,
) error {
	n := names{spec}

	if err := p.c.identity.DeleteFederatedCredential(
		ctx, n.resourceGroup(), n.identity(comp.Name), n.federatedCredential(comp.Name),
	); err != nil && code(err) != 404 {
		return fmt.Errorf("deleting federated credential for %s: %w", comp.Name, err)
	}

	if err := p.c.identity.DeleteIdentity(ctx, n.resourceGroup(), n.identity(comp.Name)); err != nil {
		if code(err) == 404 {
			return nil
		}
		return fmt.Errorf("deleting managed identity %s: %w", n.identity(comp.Name), err)
	}
	p.c.logger.Info("deleted managed identity", "identity", n.identity(comp.Name), "component", comp.Name, "cluster", spec.ID)
	return nil
}
