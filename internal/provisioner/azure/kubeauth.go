package azure

import (
	"context"
	"fmt"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

// RESTConfig builds a client config for spec's AKS API server, satisfying
// provisioner.RESTConfigProvisioner.
//
// Unlike AWS and GCP, where a bearer token is minted against a separately
// discovered endpoint and CA, AKS's ListClusterUserCredentials returns a
// complete kubeconfig — endpoint, CA data, and auth all together, including
// the exec-plugin entry an AAD-enabled cluster requires for token refresh —
// so parsing that directly is what `az aks get-credentials` itself does,
// rather than this package re-deriving the same information piecemeal.
func (p *ClusterProvisioner) RESTConfig(ctx context.Context, spec core.ClusterSpec) (*rest.Config, error) {
	n := names{spec}

	state, err := p.Describe(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("describing AKS cluster %s: %w", spec.ID, err)
	}
	if state.Status != provisioner.StatusActive {
		return nil, fmt.Errorf("AKS cluster %s is not active (status %s)", spec.ID, state.Status)
	}

	kubeconfig, err := p.c.cluster.ListClusterUserCredentials(ctx, n.resourceGroup(), n.cluster())
	if err != nil {
		return nil, fmt.Errorf("fetching kubeconfig for %s: %w", spec.ID, err)
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parsing kubeconfig for %s: %w", spec.ID, err)
	}
	return cfg, nil
}
