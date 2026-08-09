package azure

import (
	"testing"

	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

func TestRESTConfig(t *testing.T) {
	t.Run("active cluster yields a usable config", func(t *testing.T) {
		f := newFakeAzure()
		spec := testSpec()
		f.activeCluster(spec)

		cfg, err := NewClusterProvisioner(f.clients()).RESTConfig(t.Context(), spec)
		if err != nil {
			t.Fatalf("RESTConfig: %v", err)
		}
		if cfg.Host != "https://fake-aks.example.com" {
			t.Errorf("Host = %q, want the kubeconfig's server", cfg.Host)
		}
		if cfg.BearerToken != "fake-aks-token" {
			t.Errorf("BearerToken = %q, want the kubeconfig's token", cfg.BearerToken)
		}
		if string(cfg.CAData) != "fake-ca-cert" {
			t.Errorf("CAData = %q, want the decoded kubeconfig CA", cfg.CAData)
		}
		if !f.called("ListClusterUserCredentials") {
			t.Error("expected the kubeconfig to be fetched")
		}
	})

	t.Run("absent cluster is an error", func(t *testing.T) {
		f := newFakeAzure()

		_, err := NewClusterProvisioner(f.clients()).RESTConfig(t.Context(), testSpec())
		if err == nil {
			t.Fatal("expected an error for a cluster that does not exist yet")
		}
	})

	t.Run("not-yet-active cluster is an error", func(t *testing.T) {
		f := newFakeAzure()
		spec := testSpec()
		f.activeCluster(spec)
		f.cluster.Properties.ProvisioningState = ptr("Creating")

		_, err := NewClusterProvisioner(f.clients()).RESTConfig(t.Context(), spec)
		if err == nil {
			t.Fatal("expected an error for a cluster that is not active yet")
		}
	})

	_ = provisioner.RESTConfigProvisioner(&ClusterProvisioner{})
}
