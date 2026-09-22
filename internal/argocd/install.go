package argocd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/release"
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

// DefaultAddon is the catalog entry every size includes (internal/catalog's
// baseAddons), and what Install falls back to on the rare chance a resolved
// profile carries none — Argo CD has to be installed on every cluster
// regardless: app-of-apps cannot sync into a cluster that doesn't have it yet.
var DefaultAddon = core.AddonRef{
	Name:       "argocd",
	Chart:      "argo-cd",
	Repository: "https://argoproj.github.io/argo-helm",
	Version:    "10.9.2",
	Namespace:  installNamespace,
	Values:     ServerLoadBalancerValues,
}

// ServerLoadBalancerValues overlays the argo-cd chart's default server
// Service (ClusterIP) with a cloud LoadBalancer, so the Argo CD UI/API gets a
// reachable external address without a kubectl port-forward. Shared between
// DefaultAddon and every tier's tracked argocd catalog entry so they don't
// drift.
var ServerLoadBalancerValues = map[string]any{
	"server": map[string]any{
		"service": map[string]any{
			"type": "LoadBalancer",
		},
	},
}

// Installer installs or upgrades Argo CD itself into a cluster — the one
// piece of the addon pipeline that has to exist before app-of-apps can sync
// anything into it, so it is not delivered as an Argo CD Application like
// every other addon.
type Installer interface {
	// Install converges the cluster reachable via restConfig onto addon's
	// chart/version: installing it if this is the first apply, upgrading it
	// in place otherwise. It must be safe to call on every apply — a
	// no-change call performs no cluster-mutating Helm operation at all and
	// returns nil, matching every other Reconcile-shaped call in this
	// codebase.
	//
	// It returns only once Argo CD is actually running, not merely once its
	// manifests have been submitted. Callers rely on that: the caller's very
	// next act is to apply an Application, a resource type that exists only
	// because this call created its CRD.
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

	// timeout bounds the wait for the release to become ready. A field rather
	// than a constant so a caller on a slow or quota-constrained cluster can
	// raise it without patching this package.
	timeout time.Duration
}

// InstallTimeout is how long Install waits for Argo CD's workloads to become
// ready before giving up. Generous, because the wait is dominated by pulling
// Argo CD's images onto fresh nodes, and because failing a cluster that was
// merely slow would be worse than waiting: the phase is not recorded until
// this returns, so a premature failure re-runs the whole install.
const InstallTimeout = 10 * time.Minute

// NewHelmInstaller builds a HelmInstaller.
func NewHelmInstaller(logger *slog.Logger) *HelmInstaller {
	if logger == nil {
		logger = slog.Default()
	}
	return &HelmInstaller{logger: logger, timeout: InstallTimeout}
}

// waitTimeout returns the configured timeout, defaulting when the struct was
// built as a bare literal.
func (h *HelmInstaller) waitTimeout() time.Duration {
	if h.timeout <= 0 {
		return InstallTimeout
	}
	return h.timeout
}

// Install implements Installer.
func (h *HelmInstaller) Install(ctx context.Context, restConfig *rest.Config, addon core.AddonRef) error {
	cfg, err := h.actionConfig(restConfig)
	if err != nil {
		return fmt.Errorf("initialising helm: %w", err)
	}

	exists, err := h.releaseExists(cfg, ReleaseName)
	if err != nil {
		return fmt.Errorf("checking for an existing %s release: %w", ReleaseName, err)
	}

	settings := cli.New()
	if exists {
		// Skip the upgrade entirely when the deployed release already carries
		// exactly this chart version and these values. Every apply on a ready
		// cluster calls Install (orchestrator.ReadyReconcile), so without this
		// a no-change apply issued a full Helm upgrade — re-running the
		// chart's pre-upgrade hooks each time, which is how a repeat apply
		// against a healthy cluster came to fail on argo-cd's
		// redis-secret-init RBAC ("rolebindings ... already exists"). A
		// no-change apply now makes no cluster-mutating Helm call at all,
		// matching the same convergence contract every provisioner follows.
		current, err := h.deployedRelease(cfg, ReleaseName)
		switch {
		case err != nil:
			// Not fatal: an unreadable release is a reason to converge, not
			// to fail an apply. Fall through to the upgrade.
			h.logger.Warn("could not read the current argocd release; upgrading to converge",
				"release", ReleaseName, "error", err)
		case upToDate(current, addon):
			h.logger.Info("argocd already at the desired chart version and values; nothing to upgrade",
				"chart", addon.Chart, "version", addon.Version, "revision", current.Version)
			return nil
		}

		up := action.NewUpgrade(cfg)
		up.Namespace = installNamespace
		up.RepoURL = addon.Repository
		up.Version = addon.Version
		up.Install = false
		// See the Wait/Atomic reasoning on the install path below.
		up.Wait = true
		up.Timeout = h.waitTimeout()
		up.Atomic = false
		chartPath, err := up.LocateChart(addon.Chart, settings)
		if err != nil {
			return fmt.Errorf("locating chart %s: %w", addon.Chart, err)
		}
		chrt, err := loader.Load(chartPath)
		if err != nil {
			return fmt.Errorf("loading chart %s: %w", addon.Chart, err)
		}
		h.logger.Info("upgrading argocd and waiting for it to become ready",
			"chart", addon.Chart, "version", addon.Version, "timeout", h.waitTimeout())
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

	// Wait, so that "installed argocd" means Argo CD is actually running.
	// Without it Run returns once the manifests are submitted, which made
	// this step report success for a release that then never became ready —
	// an Argo CD whose pods cannot schedule or pull surfaced later as addons
	// mysteriously never syncing, rather than here, where the cause is
	// obvious. It also ordered this step ahead of itself: the root
	// Application applied moments later needs the CRDs this release creates.
	//
	// Not Atomic. A rollback would uninstall a part-working release, and the
	// phase is not recorded on failure anyway, so the retry re-enters here
	// and converges via the upgrade path above — the same create-or-update,
	// never-delete discipline every provisioner follows. Rolling back would
	// only make each attempt slower and destroy the evidence of why it hung.
	inst.Wait = true
	inst.Timeout = h.waitTimeout()
	inst.Atomic = false
	chartPath, err := inst.LocateChart(addon.Chart, settings)
	if err != nil {
		return fmt.Errorf("locating chart %s: %w", addon.Chart, err)
	}
	chrt, err := loader.Load(chartPath)
	if err != nil {
		return fmt.Errorf("loading chart %s: %w", addon.Chart, err)
	}
	// Pulling Argo CD's images onto fresh nodes takes minutes; say so rather
	// than looking hung.
	h.logger.Info("installing argocd and waiting for it to become ready; this takes a few minutes",
		"chart", addon.Chart, "version", addon.Version, "timeout", h.waitTimeout())
	if _, err := inst.RunWithContext(ctx, chrt, addon.Values); err != nil {
		return fmt.Errorf("installing %s: %w", ReleaseName, err)
	}
	h.logger.Info("installed argocd release", "chart", addon.Chart, "version", addon.Version)
	return nil
}

// deployedRelease returns the release's latest revision, whatever its status.
func (h *HelmInstaller) deployedRelease(cfg *action.Configuration, releaseName string) (*release.Release, error) {
	rel, err := action.NewGet(cfg).Run(releaseName)
	if err != nil {
		return nil, fmt.Errorf("reading release %s: %w", releaseName, err)
	}
	return rel, nil
}

// upToDate reports whether rel already is exactly what addon asks for, so
// that Install can return without touching the cluster. Deliberately strict:
// anything it cannot positively confirm — a release mid-failure, an unpinned
// addon version, values it cannot compare — reports false and converges,
// since a redundant upgrade is recoverable where a skipped necessary one is
// silent drift.
func upToDate(rel *release.Release, addon core.AddonRef) bool {
	if rel == nil || rel.Info == nil || rel.Chart == nil || rel.Chart.Metadata == nil {
		return false
	}
	// Only a cleanly deployed release is known-good. A failed or superseded
	// one has to be converged even if its chart reference matches.
	if rel.Info.Status != release.StatusDeployed {
		return false
	}
	// An addon pinning no version asks for "whatever is newest in the
	// repository", a question the deployed release cannot answer.
	if addon.Version == "" {
		return false
	}
	if rel.Chart.Metadata.Name != addon.Chart || rel.Chart.Metadata.Version != addon.Version {
		return false
	}
	return sameValues(rel.Config, addon.Values)
}

// sameValues compares two Helm values trees. Marshalling to JSON normalises
// what a round trip through the release store does to the types inside
// (an int becomes a float64, and so on), which a reflect.DeepEqual would
// report as a difference on every single apply; json.Marshal also orders map
// keys, so two equal trees always produce identical bytes. A tree that will
// not marshal (YAML's map[any]any, say) reports false and converges.
func sameValues(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	aj, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bj, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(aj, bj)
}

// releaseExists reports whether releaseName already has a Helm release
// history, so Install can route to Upgrade rather than a fresh Install.
//
// It also recovers a release stuck in a Pending* status: a run interrupted
// mid-install/upgrade (a lost connection, an expired auth token during a long
// wait for fresh nodes to become ready) leaves the release's latest revision
// pending forever, since Helm only records success or failure once its own
// action returns — and every future install/upgrade against that release then
// fails outright with "another operation ... is in progress", which a plain
// retry cannot fix. See recoverPendingRelease.
func (h *HelmInstaller) releaseExists(cfg *action.Configuration, releaseName string) (bool, error) {
	hist, err := action.NewHistory(cfg).Run(releaseName)
	switch {
	case err == nil:
		sort.Slice(hist, func(i, j int) bool { return hist[i].Version < hist[j].Version })
		latest := hist[len(hist)-1]
		switch latest.Info.Status {
		case release.StatusPendingInstall, release.StatusPendingUpgrade, release.StatusPendingRollback:
			return h.recoverPendingRelease(cfg, releaseName, hist, latest)
		default:
			return true, nil
		}
	case errors.Is(err, driver.ErrReleaseNotFound):
		return false, nil
	default:
		return false, fmt.Errorf("checking release history for %s: %w", releaseName, err)
	}
}

// recoverPendingRelease clears a release stuck mid-operation from an earlier
// interrupted run.
//
// If a prior successful revision exists, rolling back to it restores a known
// good state and lets the normal upgrade path take over (reports the release
// still exists). Otherwise — the pending revision is the release's first and
// only one, so there is nothing to roll back to — the stuck release is
// uninstalled entirely, and the normal install path creates it fresh
// (reports the release no longer exists).
func (h *HelmInstaller) recoverPendingRelease(
	cfg *action.Configuration, releaseName string, hist []*release.Release, latest *release.Release,
) (bool, error) {
	h.logger.Warn("release stuck mid-operation from an earlier interrupted run; recovering",
		"release", releaseName, "status", latest.Info.Status, "revision", latest.Version)

	lastDeployed := lastDeployedRevision(hist)

	if lastDeployed == nil {
		if _, err := action.NewUninstall(cfg).Run(releaseName); err != nil && !errors.Is(err, driver.ErrReleaseNotFound) {
			return false, fmt.Errorf("uninstalling stuck %s release: %w", releaseName, err)
		}
		h.logger.Info("uninstalled stuck release; will install fresh", "release", releaseName)
		return false, nil
	}

	rb := action.NewRollback(cfg)
	rb.Version = lastDeployed.Version
	if err := rb.Run(releaseName); err != nil {
		return false, fmt.Errorf("rolling back stuck %s release to revision %d: %w", releaseName, lastDeployed.Version, err)
	}
	h.logger.Info("rolled back stuck release to its last good revision",
		"release", releaseName, "revision", lastDeployed.Version)
	return true, nil
}

// lastDeployedRevision returns the highest-versioned revision in hist whose
// status is Deployed, or nil if none is: a release stuck on its very first
// (Pending*) revision has never had a successful one. Pure and
// side-effect-free — recoverPendingRelease is the thin wrapper that acts on
// what this decides, which is what keeps the decision itself unit-testable
// without a live cluster.
func lastDeployedRevision(hist []*release.Release) *release.Release {
	var lastDeployed *release.Release
	for _, r := range hist {
		if r.Info.Status == release.StatusDeployed {
			lastDeployed = r
		}
	}
	return lastDeployed
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
