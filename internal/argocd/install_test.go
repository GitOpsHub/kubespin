package argocd

import (
	"testing"

	"k8s.io/client-go/rest"

	"github.com/GitOpsHub/kubespin/internal/core"
)

func TestHelmInstaller_ImplementsInstaller(t *testing.T) {
	var _ Installer = NewHelmInstaller(nil)
}

func TestReleaseExists_TreatsUnreachableAsAnError(t *testing.T) {
	// releaseExists' job is narrow: translate driver.ErrReleaseNotFound into
	// false and pass every other error through unchanged. A cluster this
	// package cannot reach — the case here, with no live cluster available —
	// is exactly the "every other error" branch; it must not be swallowed
	// into a false "no release yet" that would make Install choose Install
	// over Upgrade against a cluster that already has one.
	h := NewHelmInstaller(nil)
	cfg, err := h.actionConfig(&restConfigStub)
	if err != nil {
		t.Fatalf("actionConfig: %v", err)
	}

	if _, err := releaseExists(cfg, ReleaseName); err == nil {
		t.Fatal("expected an error against an unreachable cluster, not a false negative")
	}
}

func TestStaticRESTClientGetter_ReturnsTheGivenConfig(t *testing.T) {
	g := &staticRESTClientGetter{cfg: &restConfigStub}

	cfg, err := g.ToRESTConfig()
	if err != nil {
		t.Fatalf("ToRESTConfig: %v", err)
	}
	if cfg != &restConfigStub {
		t.Error("ToRESTConfig did not return the exact config it was built with")
	}
	if g.ToRawKubeConfigLoader() == nil {
		t.Error("ToRawKubeConfigLoader returned nil")
	}
}

func TestInstall_MissingRepositoryIsAnError(t *testing.T) {
	h := NewHelmInstaller(nil)

	err := h.Install(t.Context(), &restConfigStub, core.AddonRef{
		Name: "argocd", Chart: "argo-cd", Namespace: Namespace, Version: "7.7.11",
	})
	if err == nil {
		t.Fatal("expected LocateChart to fail without a repository or network access to one")
	}
}

// restConfigStub is enough of a *rest.Config for action.Configuration.Init to
// build discovery/REST-mapper clients against; nothing in this file's tests
// makes a network call through it.
var restConfigStub = rest.Config{Host: "https://127.0.0.1:6443"}
