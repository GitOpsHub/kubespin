package argocd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// ReleaseName and Namespace are fixed: every cluster gets exactly one Argo CD
// installation, self-referential to its own repository, so there is nothing
// per-cluster to parameterise beyond the addon version.
const (
	ReleaseName = "argocd"
	// Namespace is shared with internal/argocd's Application rendering
	// (appofapps.go) so the root Application's destination namespace and the
	// namespace Argo CD is actually installed into never drift apart.
	installNamespace = Namespace
)

// DefaultAddon is what Install uses when a cluster's resolved profile carries
// no "argocd" catalog entry of its own — true of tier-small today (M4's
// catalog only tracks Argo CD's version starting at tier-standard, so `fleet
// audit`/`fleet update` can pin it there). Argo CD still has to be installed
// on every tier regardless: app-of-apps cannot sync into a cluster that
// doesn't have it yet.
var DefaultAddon = core.AddonRef{
	Name:       "argocd",
	Chart:      "argo-cd",
	Repository: "https://argoproj.github.io/argo-helm",
	Version:    "7.7.11",
	Namespace:  installNamespace,
}

// Installer installs or upgrades Argo CD itself into a cluster — the one
// piece of the addon pipeline that has to exist before app-of-apps can sync
// anything into it, so it is not delivered as an Argo CD Application like
// every other addon.
type Installer interface {
	// Install converges the cluster reachable via restConfig onto addon's
	// chart/version: installing it if this is the first apply, upgrading it
	// in place otherwise. It must be safe to call on every apply — a
	// no-change call should not error, matching every other Reconcile-shaped
	// call in this codebase.
	Install(ctx context.Context, restConfig *rest.Config, addon core.AddonRef) error
}

// HelmInstaller is the real Installer, built on Helm's Go library
// (helm.sh/helm/v3/pkg/action) rather than shelling out to the helm binary —
// the same discipline every cloud SDK call in this codebase follows.
//
// It has no live-cluster test coverage: action.Install.Run and
// action.Upgrade.Run both require a reachable Kubernetes API server before
// they do anything (Configuration.KubeClient.IsReachable), which is the same
// live-infra gap every other cloud-facing package in this codebase is
// explicit about needing a real account/cluster to close. What is covered is
// the release-exists branch and the chart reference this type builds, both
// pure functions of their inputs.
type HelmInstaller struct {
	logger *slog.Logger
}

// NewHelmInstaller builds a HelmInstaller.
func NewHelmInstaller(logger *slog.Logger) *HelmInstaller {
	if logger == nil {
		logger = slog.Default()
	}
	return &HelmInstaller{logger: logger}
}

// Install implements Installer.
func (h *HelmInstaller) Install(ctx context.Context, restConfig *rest.Config, addon core.AddonRef) error {
	cfg, err := h.actionConfig(restConfig)
	if err != nil {
		return fmt.Errorf("initialising helm: %w", err)
	}

	exists, err := releaseExists(cfg, ReleaseName)
	if err != nil {
		return fmt.Errorf("checking for an existing %s release: %w", ReleaseName, err)
	}

	settings := cli.New()
	if exists {
		up := action.NewUpgrade(cfg)
		up.Namespace = installNamespace
		up.RepoURL = addon.Repository
		up.Version = addon.Version
		up.Install = false
		chartPath, err := up.LocateChart(addon.Chart, settings)
		if err != nil {
			return fmt.Errorf("locating chart %s: %w", addon.Chart, err)
		}
		chrt, err := loader.Load(chartPath)
		if err != nil {
			return fmt.Errorf("loading chart %s: %w", addon.Chart, err)
		}
		if _, err := up.RunWithContext(ctx, ReleaseName, chrt, addon.Values); err != nil {
			return fmt.Errorf("upgrading %s: %w", ReleaseName, err)
		}
		h.logger.Info("upgraded argocd release", "chart", addon.Chart, "version", addon.Version)
		return nil
	}

	inst := action.NewInstall(cfg)
	inst.ReleaseName = ReleaseName
	inst.Namespace = installNamespace
	inst.CreateNamespace = true
	inst.RepoURL = addon.Repository
	inst.Version = addon.Version
	chartPath, err := inst.LocateChart(addon.Chart, settings)
	if err != nil {
		return fmt.Errorf("locating chart %s: %w", addon.Chart, err)
	}
	chrt, err := loader.Load(chartPath)
	if err != nil {
		return fmt.Errorf("loading chart %s: %w", addon.Chart, err)
	}
	if _, err := inst.RunWithContext(ctx, chrt, addon.Values); err != nil {
		return fmt.Errorf("installing %s: %w", ReleaseName, err)
	}
	h.logger.Info("installed argocd release", "chart", addon.Chart, "version", addon.Version)
	return nil
}

// releaseExists reports whether releaseName already has a Helm release
// history, so Install can route to Upgrade rather than a fresh Install.
func releaseExists(cfg *action.Configuration, releaseName string) (bool, error) {
	_, err := action.NewHistory(cfg).Run(releaseName)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, driver.ErrReleaseNotFound):
		return false, nil
	default:
		return false, fmt.Errorf("checking release history for %s: %w", releaseName, err)
	}
}

// actionConfig builds a Helm action.Configuration addressed at restConfig,
// storing release state as Secrets in installNamespace the same way `helm`
// itself defaults to.
func (h *HelmInstaller) actionConfig(restConfig *rest.Config) (*action.Configuration, error) {
	cfg := new(action.Configuration)
	getter := &staticRESTClientGetter{cfg: restConfig}
	debugLog := func(format string, v ...any) { h.logger.Debug(fmt.Sprintf(format, v...)) }
	if err := cfg.Init(getter, installNamespace, "secret", debugLog); err != nil {
		return nil, fmt.Errorf("initialising helm action configuration: %w", err)
	}
	return cfg, nil
}

// staticRESTClientGetter adapts an already-resolved *rest.Config to the
// interface Helm's action.Configuration.Init expects
// (genericclioptions.RESTClientGetter), so Helm never has to know the config
// came from a cloud-native token mint (internal/provisioner) rather than a
// kubeconfig file on disk.
type staticRESTClientGetter struct {
	cfg *rest.Config
}

func (g *staticRESTClientGetter) ToRESTConfig() (*rest.Config, error) { return g.cfg, nil }

func (g *staticRESTClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(g.cfg)
	if err != nil {
		return nil, fmt.Errorf("building discovery client: %w", err)
	}
	return memory.NewMemCacheClient(dc), nil
}

func (g *staticRESTClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	dc, err := g.ToDiscoveryClient()
	if err != nil {
		return nil, err // already wrapped by ToDiscoveryClient
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(dc)
	return restmapper.NewShortcutExpander(mapper, dc, func(string) {}), nil
}

// ToRawKubeConfigLoader satisfies the interface but is never exercised by the
// action.Install/Upgrade/History calls this file makes — those only ever
// call ToRESTConfig, ToDiscoveryClient, and ToRESTMapper. It returns an empty
// loader rather than nil so a future caller gets a clear "no such context"
// error instead of a nil-pointer panic if that ever changes.
func (g *staticRESTClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return clientcmd.NewDefaultClientConfig(api.Config{}, &clientcmd.ConfigOverrides{})
}
