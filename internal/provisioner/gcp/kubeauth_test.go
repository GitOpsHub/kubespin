package gcp

import (
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

func TestRESTConfig(t *testing.T) {
	t.Run("active cluster yields a usable config", func(t *testing.T) {
		f := newFakeGCP()
		spec := testSpec()
		f.activeCluster(spec)

		cfg, err := NewClusterProvisioner(f.clients()).RESTConfig(t.Context(), spec)
		if err != nil {
			t.Fatalf("RESTConfig: %v", err)
		}
		if cfg.Host != "https://203.0.113.10" {
			t.Errorf("Host = %q, want the cluster's endpoint with a scheme", cfg.Host)
		}
		if cfg.BearerToken != "fake-gcp-access-token" {
			t.Errorf("BearerToken = %q, want the fake ADC token", cfg.BearerToken)
		}
		if string(cfg.CAData) != "fake-ca-cert" {
			t.Errorf("CAData = %q, want the decoded cluster CA", cfg.CAData)
		}
		if !f.called("Token") {
			t.Error("expected a bearer token to be minted")
		}
	})

	t.Run("absent cluster is an error", func(t *testing.T) {
		f := newFakeGCP()

		_, err := NewClusterProvisioner(f.clients()).RESTConfig(t.Context(), testSpec())
		if err == nil {
			t.Fatal("expected an error for a cluster that does not exist yet")
		}
	})

	t.Run("not-yet-active cluster is an error", func(t *testing.T) {
		f := newFakeGCP()
		spec := testSpec()
		f.activeCluster(spec)
		f.cluster.Status = containerpb.Cluster_PROVISIONING

		_, err := NewClusterProvisioner(f.clients()).RESTConfig(t.Context(), spec)
		if err == nil {
			t.Fatal("expected an error for a cluster that is not active yet")
		}
	})

	_ = provisioner.RESTConfigProvisioner(&ClusterProvisioner{})
}
