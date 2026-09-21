package aws

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"sync"
	"time"

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

// eksTokenExpirySeconds is stamped onto the presigned URL's X-Amz-Expires
// query parameter. aws-sdk-go-v2's PresignHTTP does *not* set it on its own
// (see its doc comment) — omitting it entirely produces a presigned URL the
// built-in authenticator rejects outright rather than one that merely never
// expires, so this has to be added explicitly.
//
// The value itself is close to decorative: aws-iam-authenticator's own
// Verify() only sanity-checks that X-Amz-Expires is between 0 and 900, then
// separately enforces a flat, hardcoded 15-minute window measured from the
// request's X-Amz-Date — it ignores whatever X-Amz-Expires actually claims
// beyond that bounds check. "60" here matches the token AWS's own
// `aws-iam-authenticator token`/`aws eks get-token` mint (which sets it for
// legacy compatibility, by its own admission unused), so it is kept for
// parity rather than because it changes the real expiry. The real,
// enforced window is what eksTokenRefreshInterval below has to stay under.
const eksTokenExpirySeconds = "60"

// eksTokenRefreshInterval bounds how long a minted bearer token is reused
// before RESTConfig's transport mints a fresh one. It must stay comfortably
// under aws-iam-authenticator's actual enforced window (a flat 15 minutes
// from the token's signing time, per its Verify() — see eksTokenExpirySeconds
// above); 10 minutes leaves a 5-minute margin for a slow request or clock
// skew. A single static token was the original design here, and it broke on
// EKS Auto Mode: node provisioning on first pod schedule can leave the Argo
// CD Helm install's wait-for-ready loop running for 15+ minutes, well past
// the real window, so every request after that point failed with
// Unauthorized. Standard EKS with pre-warmed managed node groups usually
// finishes well inside 15 minutes, which is why this was not caught earlier.
const eksTokenRefreshInterval = 10 * time.Minute

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
//
// The returned config carries no static bearer token. A long-running caller
// (Argo CD's Helm install, which can legitimately run 15+ minutes while
// nodes provision) would otherwise present the same token past its real
// ~15-minute validity window and start failing with Unauthorized partway
// through — WrapTransport instead mints a fresh token on first use and
// re-mints it every eksTokenRefreshInterval, transparently to every caller
// that just wants a *rest.Config.
func (p *ClusterProvisioner) RESTConfig(ctx context.Context, spec core.ClusterSpec) (*rest.Config, error) {
	state, err := p.Describe(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("describing EKS cluster %s: %w", spec.ID, err)
	}
	if state.Status != provisioner.StatusActive {
		return nil, fmt.Errorf("EKS cluster %s is not active (status %s)", spec.ID, state.Status)
	}

	clusterName := names{spec}.cluster()
	cfg := &rest.Config{
		Host:            state.Endpoint,
		TLSClientConfig: rest.TLSClientConfig{CAData: state.CertificateAuthorityData},
	}
	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return &eksRefreshingTransport{
			base:    rt,
			sts:     p.c.sts,
			cluster: clusterName,
			now:     time.Now,
		}
	}
	return cfg, nil
}

// eksRefreshingTransport wraps an http.RoundTripper, minting a fresh EKS
// bearer token on the first request and again every eksTokenRefreshInterval,
// so a client holding this *rest.Config across a long-running operation
// never presents a token past its real validity window.
type eksRefreshingTransport struct {
	base    http.RoundTripper
	sts     stsPresignAPI
	cluster string

	// now is a seam for tests to simulate elapsed time without sleeping.
	now func() time.Time

	mu       sync.Mutex
	token    string
	mintedAt time.Time
}

func (t *eksRefreshingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := t.currentToken(req.Context())
	if err != nil {
		return nil, fmt.Errorf("minting EKS bearer token for %s: %w", t.cluster, err)
	}

	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("round-tripping request for %s: %w", t.cluster, err)
	}
	return resp, nil
}

func (t *eksRefreshingTransport) currentToken(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.token != "" && t.now().Sub(t.mintedAt) < eksTokenRefreshInterval {
		return t.token, nil
	}

	url, err := t.sts.PresignGetCallerIdentityURL(ctx, t.cluster)
	if err != nil {
		return "", fmt.Errorf("presigning GetCallerIdentity for %s: %w", t.cluster, err)
	}
	t.token = eksTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(url))
	t.mintedAt = t.now()
	return t.token, nil
}
