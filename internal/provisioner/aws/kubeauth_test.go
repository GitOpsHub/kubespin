package aws

import (
	"strings"
	"testing"

	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

func TestRESTConfig(t *testing.T) {
	t.Run("active cluster yields a usable config", func(t *testing.T) {
		f := newFakeAWS()
		spec := testSpec()
		f.activeCluster(spec)

		cfg, err := NewClusterProvisioner(f.clients()).RESTConfig(t.Context(), spec)
		if err != nil {
			t.Fatalf("RESTConfig: %v", err)
		}
		if cfg.Host != "https://example.eks.amazonaws.com" {
			t.Errorf("Host = %q, want the cluster's endpoint", cfg.Host)
		}
		if !strings.HasPrefix(cfg.BearerToken, eksTokenPrefix) {
			t.Errorf("BearerToken = %q, want it prefixed with %q", cfg.BearerToken, eksTokenPrefix)
		}
		if string(cfg.CAData) != "fake-ca-cert" {
			t.Errorf("CAData = %q, want the decoded cluster CA", cfg.CAData)
		}
		if !f.called("PresignGetCallerIdentity") {
			t.Error("expected a bearer token to be minted via STS")
		}
	})

	t.Run("absent cluster is an error", func(t *testing.T) {
		f := newFakeAWS()

		_, err := NewClusterProvisioner(f.clients()).RESTConfig(t.Context(), testSpec())
		if err == nil {
			t.Fatal("expected an error for a cluster that does not exist yet")
		}
	})

	t.Run("not-yet-active cluster is an error", func(t *testing.T) {
		f := newFakeAWS()
		spec := testSpec()
		f.activeCluster(spec)
		f.cluster.Status = ekstypes.ClusterStatusCreating

		_, err := NewClusterProvisioner(f.clients()).RESTConfig(t.Context(), spec)
		if err == nil {
			t.Fatal("expected an error for a cluster that is not active yet")
		}
	})

	_ = provisioner.RESTConfigProvisioner(&ClusterProvisioner{})
}
