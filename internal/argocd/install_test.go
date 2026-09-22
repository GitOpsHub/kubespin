package argocd

import (
	"testing"
	"time"

	helmchart "helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
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

	if _, err := h.releaseExists(cfg, ReleaseName); err == nil {
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
		Name: "argocd", Chart: "argo-cd", Namespace: Namespace, Version: "10.9.2",
	})
	if err == nil {
		t.Fatal("expected LocateChart to fail without a repository or network access to one")
	}
}

// restConfigStub is enough of a *rest.Config for action.Configuration.Init to
// build discovery/REST-mapper clients against; nothing in this file's tests
// makes a network call through it.
var restConfigStub = rest.Config{Host: "https://127.0.0.1:6443"}

// TestLastDeployedRevision covers the decision recoverPendingRelease acts
// on: whether a release stuck in a Pending* status has a prior successful
// revision to roll back to, or has to be uninstalled and reinstalled fresh.
// Pure and side-effect-free, so it needs no live cluster.
func TestLastDeployedRevision(t *testing.T) {
	t.Run("no deployed revision, only the stuck first one", func(t *testing.T) {
		hist := []*release.Release{
			{Version: 1, Info: &release.Info{Status: release.StatusPendingInstall}},
		}
		if got := lastDeployedRevision(hist); got != nil {
			t.Errorf("lastDeployedRevision = %+v, want nil", got)
		}
	})

	t.Run("a prior deployed revision exists", func(t *testing.T) {
		hist := []*release.Release{
			{Version: 1, Info: &release.Info{Status: release.StatusDeployed}},
			{Version: 2, Info: &release.Info{Status: release.StatusSuperseded}},
			{Version: 3, Info: &release.Info{Status: release.StatusPendingUpgrade}},
		}
		got := lastDeployedRevision(hist)
		if got == nil || got.Version != 1 {
			t.Errorf("lastDeployedRevision = %+v, want revision 1", got)
		}
	})

	t.Run("multiple deployed revisions returns the highest", func(t *testing.T) {
		hist := []*release.Release{
			{Version: 1, Info: &release.Info{Status: release.StatusSuperseded}},
			{Version: 2, Info: &release.Info{Status: release.StatusDeployed}},
			{Version: 3, Info: &release.Info{Status: release.StatusPendingRollback}},
		}
		got := lastDeployedRevision(hist)
		if got == nil || got.Version != 2 {
			t.Errorf("lastDeployedRevision = %+v, want revision 2", got)
		}
	})
}

// TestHelmInstaller_WaitTimeout covers the readiness-wait bound. Install
// blocks until Argo CD is actually running, so a zero timeout on a
// bare-literal HelmInstaller would mean "no bound at all" — Helm treats 0 as
// no deadline, which would let a stuck install hang an apply indefinitely.
func TestHelmInstaller_WaitTimeout(t *testing.T) {
	if got := NewHelmInstaller(nil).waitTimeout(); got != InstallTimeout {
		t.Errorf("waitTimeout = %s, want %s", got, InstallTimeout)
	}

	// Built as a bare struct literal, as this package's own tests do.
	if got := (&HelmInstaller{}).waitTimeout(); got != InstallTimeout {
		t.Errorf("waitTimeout on a zero-value installer = %s, want the default %s", got, InstallTimeout)
	}

	if got := (&HelmInstaller{timeout: time.Minute}).waitTimeout(); got != time.Minute {
		t.Errorf("waitTimeout = %s, want the configured 1m", got)
	}
}

// TestUpToDate covers the decision that keeps a no-change apply from issuing
// a Helm upgrade at all — the thing that made a repeat apply re-run argo-cd's
// pre-upgrade hooks and fail on RBAC left behind by its own install. Pure, so
// it needs no live cluster.
func TestUpToDate(t *testing.T) {
	deployed := func(chart, version string, values map[string]any) *release.Release {
		return &release.Release{
			Version: 3,
			Info:    &release.Info{Status: release.StatusDeployed},
			Chart:   &helmchart.Chart{Metadata: &helmchart.Metadata{Name: chart, Version: version}},
			Config:  values,
		}
	}
	addon := core.AddonRef{
		Name: "argocd", Chart: "argo-cd", Version: "10.9.2",
		Namespace: Namespace, Values: ServerLoadBalancerValues,
	}

	t.Run("same chart, version and values", func(t *testing.T) {
		if !upToDate(deployed("argo-cd", "10.9.2", ServerLoadBalancerValues), addon) {
			t.Error("upToDate = false, want true: an unchanged release must not be upgraded")
		}
	})

	t.Run("values that only differ by type after a store round trip", func(t *testing.T) {
		// The release store hands back numbers as float64; the catalog holds
		// them as ints. A reflect.DeepEqual would call these different and
		// upgrade on every apply forever.
		a := core.AddonRef{Name: "argocd", Chart: "argo-cd", Version: "10.9.2",
			Values: map[string]any{"replicas": 2}}
		rel := deployed("argo-cd", "10.9.2", map[string]any{"replicas": float64(2)})
		if !upToDate(rel, a) {
			t.Error("upToDate = false, want true: int/float64 is a round-trip artifact, not a change")
		}
	})

	t.Run("version bump", func(t *testing.T) {
		if upToDate(deployed("argo-cd", "10.9.1", ServerLoadBalancerValues), addon) {
			t.Error("upToDate = true, want false: a pinned version change must upgrade")
		}
	})

	t.Run("values change", func(t *testing.T) {
		rel := deployed("argo-cd", "10.9.2", map[string]any{
			"server": map[string]any{"service": map[string]any{"type": "ClusterIP"}},
		})
		if upToDate(rel, addon) {
			t.Error("upToDate = true, want false: a values change must reach the live release")
		}
	})

	t.Run("release not cleanly deployed", func(t *testing.T) {
		rel := deployed("argo-cd", "10.9.2", ServerLoadBalancerValues)
		rel.Info.Status = release.StatusFailed
		if upToDate(rel, addon) {
			t.Error("upToDate = true, want false: a failed release must be converged")
		}
	})

	t.Run("addon pins no version", func(t *testing.T) {
		unpinned := addon
		unpinned.Version = ""
		if upToDate(deployed("argo-cd", "10.9.2", ServerLoadBalancerValues), unpinned) {
			t.Error("upToDate = true, want false: 'newest in the repository' cannot be answered from the release")
		}
	})

	t.Run("nil and half-built releases", func(t *testing.T) {
		if upToDate(nil, addon) {
			t.Error("upToDate(nil) = true, want false")
		}
		if upToDate(&release.Release{Info: &release.Info{Status: release.StatusDeployed}}, addon) {
			t.Error("upToDate on a release with no chart = true, want false")
		}
	})
}

func TestSameValues_BothEmpty(t *testing.T) {
	if !sameValues(nil, map[string]any{}) {
		t.Error("sameValues(nil, empty) = false, want true")
	}
}
