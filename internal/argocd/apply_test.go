package argocd

import (
	"testing"

	"k8s.io/client-go/rest"
)

func TestDynamicApplier_ImplementsKubeApplier(t *testing.T) {
	var _ KubeApplier = NewDynamicApplier(nil)
}

func TestDynamicApplier_Apply_InvalidManifestIsAnError(t *testing.T) {
	a := NewDynamicApplier(nil)

	err := a.Apply(t.Context(), &rest.Config{Host: "https://127.0.0.1:6443"}, []byte("not: [valid"))
	if err == nil {
		t.Fatal("expected an error parsing an invalid manifest")
	}
}

func TestDynamicApplier_Apply_UnreachableClusterIsAnError(t *testing.T) {
	// Same live-infra gap as HelmInstaller: Apply needs a reachable API
	// server to resolve a REST mapping, which this test environment does not
	// have. What's verified is that a valid root Application manifest parses
	// far enough to attempt that call, rather than failing earlier.
	rendered, err := RenderRootApplication("https://github.com/example/cluster-repo.git")
	if err != nil {
		t.Fatalf("RenderRootApplication: %v", err)
	}

	a := NewDynamicApplier(nil)
	if err := a.Apply(t.Context(), &rest.Config{Host: "https://127.0.0.1:6443"}, rendered); err == nil {
		t.Fatal("expected an error against an unreachable cluster")
	}
}
