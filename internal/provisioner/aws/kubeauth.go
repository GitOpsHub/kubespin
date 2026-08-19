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
// cached.
const eksTokenPrefix = "k8s-aws-v1." //nolint:gosec // not a credential, just the token's format prefix

// eksTokenExpirySeconds is the token's lifetime, matching the 60s
// aws-iam-authenticator itself uses. aws-sdk-go-v2's PresignHTTP does *not*
// set X-Amz-Expires on its own (see its doc comment) — omitting it entirely
// produces a presigned URL the built-in authenticator rejects outright rather
// than one that merely never expires, so this has to be added explicitly.
const eksTokenExpirySeconds = "60"

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
// presigned for a different cluster name. It also stamps X-Amz-Expires,
// without which the presigned URL has no expiry parameter at all rather than
// one that merely never expires — aws-iam-authenticator rejects a token
// missing it outright.
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

								query := req.URL.Query()
								query.Set("X-Amz-Expires", eksTokenExpirySeconds)
								req.URL.RawQuery = query.Encode()
							}
							return next.HandleBuild(ctx, in)
						}), smithymiddleware.Before)
				})
			})
		},
	)
	if err != nil {
		return "", fmt.Errorf("presigning GetCallerIdentity for %s: %w", clusterName, err)
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
