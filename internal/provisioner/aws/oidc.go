package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

// ensureOIDCProvider registers the cluster's issuer with IAM if it is not
// already known. Every cluster has its own issuer, so this is per-cluster.
func (p *ClusterProvisioner) ensureOIDCProvider(ctx context.Context, issuer string) (string, error) {
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
func (p *ClusterProvisioner) findOIDCProvider(ctx context.Context, host string) (string, error) {
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
