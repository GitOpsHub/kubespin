package gcp

import (
	"context"
	"fmt"

	"golang.org/x/oauth2/google"
	"k8s.io/client-go/rest"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

// cloudPlatformScope is the OAuth scope the GKE API server's Google-issued
// bearer-token authenticator accepts, the same scope `gcloud container
// clusters get-credentials` requests.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// tokenAPI mints the bearer token GKE's authenticator webhook accepts.
// Narrowed to this one operation, like every other cloud call in this
// package, so RESTConfig is testable without real Google credentials.
type tokenAPI interface {
	Token(ctx context.Context) (string, error)
}

// applicationDefaultTokens is the real tokenAPI: whatever `gcloud auth
// application-default login` (or a workload's own ambient credentials)
// already cached. No credential is minted or stored by this package itself.
type applicationDefaultTokens struct{}

func (applicationDefaultTokens) Token(ctx context.Context) (string, error) {
	creds, err := google.FindDefaultCredentials(ctx, cloudPlatformScope)
	if err != nil {
		return "", fmt.Errorf("finding application default credentials: %w", err)
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("minting access token: %w", err)
	}
	return tok.AccessToken, nil
}

// RESTConfig builds a client config for spec's GKE API server, satisfying
// provisioner.RESTConfigProvisioner. The cluster must already be active: its
// endpoint and CA data come from the same Describe call every other caller
// uses.
func (p *ClusterProvisioner) RESTConfig(ctx context.Context, spec core.ClusterSpec) (*rest.Config, error) {
	state, err := p.Describe(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("describing GKE cluster %s: %w", spec.ID, err)
	}
	if state.Status != provisioner.StatusActive {
		return nil, fmt.Errorf("GKE cluster %s is not active (status %s)", spec.ID, state.Status)
	}

	token, err := p.c.tokens.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("minting GKE bearer token for %s: %w", spec.ID, err)
	}

	return &rest.Config{
		Host:            "https://" + state.Endpoint,
		BearerToken:     token,
		TLSClientConfig: rest.TLSClientConfig{CAData: state.CertificateAuthorityData},
	}, nil
}
