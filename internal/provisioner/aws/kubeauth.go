package aws

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithymiddleware "github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"k8s.io/client-go/rest"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

// eksTokenPrefix matches the token aws-iam-authenticator (built into every
// EKS control plane) accepts: a presigned STS GetCallerIdentity URL tagged
// with the target cluster name, exactly what `aws eks get-token` produces.
// Minting this in-process means no static credential is ever written down —
// the token is derived fresh from whatever session `kubespin login` already
// cached. Its lifetime is bounded by the presigned URL's own X-Amz-Expires
// (PresignGetCallerIdentity's 60s default), the same default
// aws-iam-authenticator itself uses.
const eksTokenPrefix = "k8s-aws-v1."

// stsPresignAPI mints that bearer token. Narrowed to this one operation so
// the whole RESTConfig path is testable without AWS credentials, the same way
// every other cloud call in this package is.
type stsPresignAPI interface {
	PresignGetCallerIdentityURL(ctx context.Context, clusterName string) (string, error)
}

type stsPresigner struct {
	client *sts.PresignClient
}

func newSTSPresigner(cfg aws.Config) *stsPresigner {
	return &stsPresigner{client: sts.NewPresignClient(sts.NewFromConfig(cfg))}
}

// PresignGetCallerIdentityURL presigns a GetCallerIdentity request tagged
// with clusterName via the x-k8s-aws-id header, which is what scopes the
// resulting token to that one cluster: aws-iam-authenticator refuses a token
// presigned for a different cluster name.
func (p *stsPresigner) PresignGetCallerIdentityURL(ctx context.Context, clusterName string) (string, error) {
	presigned, err := p.client.PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{},
		func(po *sts.PresignOptions) {
			po.ClientOptions = append(po.ClientOptions, func(o *sts.Options) {
				o.APIOptions = append(o.APIOptions, func(stack *smithymiddleware.Stack) error {
					return stack.Build.Add(smithymiddleware.BuildMiddlewareFunc("EKSClusterIDHeader",
						func(
							ctx context.Context, in smithymiddleware.BuildInput, next smithymiddleware.BuildHandler,
						) (smithymiddleware.BuildOutput, smithymiddleware.Metadata, error) {
							if req, ok := in.Request.(*smithyhttp.Request); ok {
								req.Header.Set("x-k8s-aws-id", clusterName)
							}
							return next.HandleBuild(ctx, in)
						}), smithymiddleware.Before)
				})
			})
		},
	)
	if err != nil {
		return "", err
	}
	return presigned.URL, nil
}

// RESTConfig builds a client config for spec's API server, satisfying
// provisioner.RESTConfigProvisioner. The cluster must already be active: its
// endpoint and CA data come from the same Describe call every other caller
// uses.
func (p *ClusterProvisioner) RESTConfig(ctx context.Context, spec core.ClusterSpec) (*rest.Config, error) {
	state, err := p.Describe(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("describing EKS cluster %s: %w", spec.ID, err)
	}
	if state.Status != provisioner.StatusActive {
		return nil, fmt.Errorf("EKS cluster %s is not active (status %s)", spec.ID, state.Status)
	}

	url, err := p.c.sts.PresignGetCallerIdentityURL(ctx, names{spec}.cluster())
	if err != nil {
		return nil, fmt.Errorf("minting EKS bearer token for %s: %w", spec.ID, err)
	}

	return &rest.Config{
		Host:            state.Endpoint,
		BearerToken:     eksTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(url)),
		TLSClientConfig: rest.TLSClientConfig{CAData: state.CertificateAuthorityData},
	}, nil
}
