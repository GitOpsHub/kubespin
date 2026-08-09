package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

// IdentityProvisioner binds in-cluster service accounts to IAM roles via IRSA.
type IdentityProvisioner struct {
	c       *Clients
	cluster *ClusterProvisioner
}

// NewIdentityProvisioner builds an IRSA provisioner.
func NewIdentityProvisioner(c *Clients) *IdentityProvisioner {
	return &IdentityProvisioner{c: c, cluster: NewClusterProvisioner(c)}
}

// Provider identifies this implementation's cloud.
func (p *IdentityProvisioner) Provider() core.Provider { return core.ProviderAWS }

// ProvisionForComponent registers the cluster's OIDC provider if needed and
// creates the component's IRSA role.
//
// The role carries no permission policy. The identity exists so the component
// can *prove* which cluster it is when it pushes status; granting it AWS access
// would be a separate, deliberate decision.
func (p *IdentityProvisioner) ProvisionForComponent(
	ctx context.Context, spec core.ClusterSpec, comp provisioner.Component,
) (provisioner.Binding, error) {
	state, err := p.cluster.Describe(ctx, spec)
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

	providerARN, err := p.ensureOIDCProvider(ctx, state.OIDCIssuer)
	if err != nil {
		return provisioner.Binding{}, err
	}

	roleARN, err := p.ensureIRSARole(ctx, spec, comp, providerARN, state.OIDCIssuer)
	if err != nil {
		return provisioner.Binding{}, err
	}

	return provisioner.Binding{
		Identifier: roleARN,
		// The caller applies this blind: each cloud uses a different key, and
		// the caller should not have to know which cloud it is on.
		Annotations: map[string]string{"eks.amazonaws.com/role-arn": roleARN},
	}, nil
}

// ensureOIDCProvider registers the cluster's issuer with IAM if it is not
// already known. Every cluster has its own issuer, so this is per-cluster.
func (p *IdentityProvisioner) ensureOIDCProvider(ctx context.Context, issuer string) (string, error) {
	listed, err := p.c.iam.ListOpenIDConnectProviders(ctx, &iam.ListOpenIDConnectProvidersInput{})
	if err != nil {
		return "", fmt.Errorf("listing OIDC providers: %w", err)
	}

	host := strings.TrimPrefix(issuer, "https://")
	for _, entry := range listed.OpenIDConnectProviderList {
		arn := aws.ToString(entry.Arn)

		got, err := p.c.iam.GetOpenIDConnectProvider(ctx, &iam.GetOpenIDConnectProviderInput{
			OpenIDConnectProviderArn: aws.String(arn),
		})
		if err != nil {
			return "", fmt.Errorf("describing OIDC provider %s: %w", arn, err)
		}
		if aws.ToString(got.Url) == host {
			return arn, nil
		}
	}

	created, err := p.c.iam.CreateOpenIDConnectProvider(ctx, &iam.CreateOpenIDConnectProviderInput{
		Url:            aws.String(issuer),
		ClientIDList:   []string{eksOIDCClientIDAudience},
		ThumbprintList: []string{eksOIDCThumbprint},
	})
	if err != nil {
		// A concurrent run may have registered it between the list and here.
		var exists *iamtypes.EntityAlreadyExistsException
		if errors.As(err, &exists) {
			return p.findOIDCProvider(ctx, host)
		}
		return "", fmt.Errorf("creating OIDC provider for %s: %w", issuer, err)
	}
	p.c.logger.Info("registered OIDC provider", "issuer", issuer)
	return aws.ToString(created.OpenIDConnectProviderArn), nil
}

func (p *IdentityProvisioner) findOIDCProvider(ctx context.Context, host string) (string, error) {
	listed, err := p.c.iam.ListOpenIDConnectProviders(ctx, &iam.ListOpenIDConnectProvidersInput{})
	if err != nil {
		return "", fmt.Errorf("listing OIDC providers: %w", err)
	}

	for _, entry := range listed.OpenIDConnectProviderList {
		arn := aws.ToString(entry.Arn)

		got, err := p.c.iam.GetOpenIDConnectProvider(ctx, &iam.GetOpenIDConnectProviderInput{
			OpenIDConnectProviderArn: aws.String(arn),
		})
		if err != nil {
			return "", fmt.Errorf("describing OIDC provider %s: %w", arn, err)
		}
		if aws.ToString(got.Url) == host {
			return arn, nil
		}
	}
	return "", fmt.Errorf("OIDC provider for %s was reported to exist but could not be found", host)
}

// ensureIRSARole creates or corrects the component's role.
func (p *IdentityProvisioner) ensureIRSARole(
	ctx context.Context, spec core.ClusterSpec, comp provisioner.Component, providerARN, issuer string,
) (string, error) {
	name := names{spec}.irsaRole(comp.Name)
	trust := irsaTrustPolicy(providerARN, issuer, comp)

	doc, err := json.Marshal(trust)
	if err != nil {
		return "", fmt.Errorf("rendering trust policy for %s: %w", name, err)
	}

	out, err := p.c.iam.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	if err == nil {
		// The trust policy is rewritten rather than compared: it is the only
		// thing standing between this role and any other service account in the
		// cluster, so drift in it is a privilege escalation.
		if _, err := p.c.iam.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{
			RoleName:       aws.String(name),
			PolicyDocument: aws.String(string(doc)),
		}); err != nil {
			return "", fmt.Errorf("updating trust policy for %s: %w", name, err)
		}
		p.c.logger.Debug("refreshed IRSA trust policy", "role", name)
		return aws.ToString(out.Role.Arn), nil
	}

	var missing *iamtypes.NoSuchEntityException
	if !errors.As(err, &missing) {
		return "", fmt.Errorf("getting role %s: %w", name, err)
	}

	created, err := p.c.iam.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String(name),
		AssumeRolePolicyDocument: aws.String(string(doc)),
		Description: aws.String(fmt.Sprintf(
			"kubespin workload identity for %s in cluster %s", comp.Name, spec.ID)),
	})
	if err != nil {
		return "", fmt.Errorf("creating role %s: %w", name, err)
	}
	p.c.logger.Info("created IRSA role", "role", name, "component", comp.Name, "cluster", spec.ID)
	return aws.ToString(created.Role.Arn), nil
}

// irsaTrustPolicy scopes the role to exactly one service account in one
// namespace of one cluster.
//
// Both conditions matter. Without `sub`, any service account in the cluster
// could assume the role; without `aud`, a token minted for another audience
// would be accepted.
func irsaTrustPolicy(providerARN, issuer string, comp provisioner.Component) map[string]any {
	host := strings.TrimPrefix(issuer, "https://")

	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect":    "Allow",
			"Action":    "sts:AssumeRoleWithWebIdentity",
			"Principal": map[string]any{"Federated": providerARN},
			"Condition": map[string]any{
				"StringEquals": map[string]any{
					host + ":sub": fmt.Sprintf("system:serviceaccount:%s:%s",
						comp.Namespace, comp.ServiceAccount),
					host + ":aud": eksOIDCClientIDAudience,
				},
			},
		}},
	}
}

// Deprovision removes the component's role.
//
// The OIDC provider is deliberately left in place: it belongs to the cluster,
// not to this component, and other components may still depend on it. Cluster
// teardown removes it.
func (p *IdentityProvisioner) Deprovision(
	ctx context.Context, spec core.ClusterSpec, comp provisioner.Component,
) error {
	name := names{spec}.irsaRole(comp.Name)

	attached, err := p.c.iam.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{
		RoleName: aws.String(name),
	})
	if err != nil {
		var missing *iamtypes.NoSuchEntityException
		if errors.As(err, &missing) {
			return nil
		}
		return fmt.Errorf("listing policies on %s: %w", name, err)
	}

	// IAM refuses to delete a role that still has policies attached, so an
	// orphaned role would survive teardown if this step were skipped.
	for _, policy := range attached.AttachedPolicies {
		if _, err := p.c.iam.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{
			RoleName:  aws.String(name),
			PolicyArn: policy.PolicyArn,
		}); err != nil {
			return fmt.Errorf("detaching %s from %s: %w", aws.ToString(policy.PolicyArn), name, err)
		}
	}

	if _, err := p.c.iam.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(name)}); err != nil {
		var missing *iamtypes.NoSuchEntityException
		if errors.As(err, &missing) {
			return nil
		}
		return fmt.Errorf("deleting role %s: %w", name, err)
	}
	p.c.logger.Info("deleted IRSA role", "role", name, "component", comp.Name, "cluster", spec.ID)
	return nil
}
