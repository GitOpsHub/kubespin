# internal/argocd

`internal/argocd` renders the manifests that bootstrap a cluster's local Argo
CD instance and hand it the app-of-apps that syncs every addon, and installs
Argo CD itself via the Helm Go library (never by shelling out to `helm` or
`kubectl`). Addons are delivered app-of-apps: one root `Application`, applied
directly to the cluster, discovers one independent `Application` per addon
committed under `apps/` in the cluster's own repository — so addons sync and
fail independently rather than as one monolithic release. Access mode
branches ingress/Gateway addon templating: an internal load balancer is
forced unless the cluster is public *and* the addon itself requests external
exposure.

## Quick reference

| Name | Kind | File | Summary |
|---|---|---|---|
| [`KubeApplier`](#kubeapplier) | Interface | apply.go | Applies a single manifest directly to a cluster's API server. |
| [`DynamicApplier`](#dynamicapplier) | Type | apply.go | The real `KubeApplier`, built on `client-go`'s dynamic client. |
| [`NewDynamicApplier`](#dynamicapplier) | Func | apply.go | Constructs a `DynamicApplier`. |
| [`Installer`](#installer) | Interface | install.go | Installs or upgrades Argo CD itself into a cluster. |
| [`HelmInstaller`](#helminstaller) | Type | install.go | The real `Installer`, built on `helm.sh/helm/v3/pkg/action`. |
| [`NewHelmInstaller`](#helminstaller) | Func | install.go | Constructs a `HelmInstaller`. |
| [`InstallTimeout`](#package-level-constants) | Const | install.go | Default wait bound (`10 * time.Minute`) for `HelmInstaller.Install`. |
| [`DefaultAddon`](#defaultaddon) | Var | install.go | Fallback Argo CD `AddonRef` for profiles with no `"argocd"` catalog entry. |
| [`ReleaseName`](#package-level-constants) | Const | install.go | Fixed Helm release name (`"argocd"`). |
| [`Application`](#application) | Type | manifest.go | Local, minimal mirror of the Argo CD `Application` CRD. |
| [`ApplicationMetadata`](#application) | Type | manifest.go | `Application.Metadata` fields. |
| [`ApplicationSpec`](#application) | Type | manifest.go | `Application.Spec` fields. |
| [`ApplicationSource`](#application) | Type | manifest.go | `Spec.Source` fields. |
| [`ApplicationSourceHelm`](#application) | Type | manifest.go | `Source.Helm` fields. |
| [`ApplicationDestination`](#application) | Type | manifest.go | `Spec.Destination` fields. |
| [`ApplicationSyncPolicy`](#application) | Type | manifest.go | `Spec.SyncPolicy` fields. |
| [`ApplicationSyncPolicyAutomated`](#application) | Type | manifest.go | `SyncPolicy.Automated` fields. |
| [`Namespace`](#package-level-constants) | Const | manifest.go | `"argocd"` — where Argo CD and every `Application` it manages live. |
| [`Option`](#option) | Type | manifest.go | Functional option for rendering entry points. |
| [`WithLogger`](#option) | Func | manifest.go | Sets the logger option. |
| [`RenderRootApplication`](#renderrootapplication) | Func | appofapps.go | Renders the app-of-apps root `Application`. |
| [`RenderAddonApplication`](#renderaddonapplication) | Func | appofapps.go | Renders one addon's independent `Application`. |
| [`RenderAddonApplications`](#renderaddonapplications) | Func | appofapps.go | Renders every addon in a profile, keyed by commit path. |
| [`AppsDir`](#package-level-constants) | Const | appofapps.go | `"apps"` — directory addon `Application` manifests are committed under. |
| [`RenderRepoCredentialsSecret`](#renderrepocredentialssecret) | Func | repocreds.go | Renders the `repo-creds` Secret for Argo CD's repo-server. |
| [`Exposure`](#exposure) | Type | ingress.go | How an ingress/Gateway addon's load balancer is reachable. |
| [`ExposureInternal`](#exposure) | Const | ingress.go | Internal exposure value. |
| [`ExposureExternal`](#exposure) | Const | ingress.go | External exposure value. |
| [`ResolveExposure`](#resolveexposure) | Func | ingress.go | Applies the public/private-aware ingress default. |
| [`ApplyIngressDefaults`](#applyingressdefaults) | Func | ingress.go | Overlays resolved exposure onto one addon's values. |
| [`ApplyProfileIngressDefaults`](#applyprofileingressdefaults) | Func | ingress.go | Applies `ApplyIngressDefaults` across a whole profile. |

## apply.go

#### `KubeApplier`

??? abstract "`KubeApplier`"

    Interface for applying a single manifest directly to a cluster's API
    server — used for exactly one resource in this codebase: app-of-apps'
    root `Application`, which cannot be delivered by Argo CD syncing itself
    since it is never committed to the repository it manages.

    ```go
    type KubeApplier interface {
        Apply(ctx context.Context, restConfig *rest.Config, manifest []byte) error
    }
    ```

    - Invariant: `Apply` must be idempotent — applying the same manifest
      twice converges rather than erroring or duplicating.

#### `DynamicApplier`

??? abstract "`DynamicApplier`"

    The real `KubeApplier`, built on `client-go`'s dynamic client and a
    discovery-backed REST mapper rather than shelling out to `kubectl` — the
    same discipline `HelmInstaller` follows for Argo CD's own install.

    ```go
    type DynamicApplier struct {
        // unexported: logger, crdInterval, crdTimeout
    }

    func NewDynamicApplier(logger *slog.Logger) *DynamicApplier
    func (a *DynamicApplier) Apply(ctx context.Context, restConfig *rest.Config, manifest []byte) error
    ```

    - `Apply` unmarshals the manifest, resolves its REST mapping (retrying
      while a just-installed CRD is not yet served by discovery — see
      `restMapping` below), and server-side-applies it with
      `FieldManager: "kubespin"` and `Force: true`.
    - `crdInterval`/`crdTimeout` (defaults `crdEstablishInterval` = 2s,
      `crdEstablishTimeout` = 90s) bound the wait for a just-installed CRD
      (e.g. `argoproj.io/v1alpha1.Application`) to become queryable via
      discovery. Only a `meta.IsNoMatchError` is retried, and the cached
      discovery document is reset before each retry — a stale cache, not a
      genuinely missing kind, is the real reason the mapping looks absent
      immediately after `HelmInstaller` installs Argo CD's CRDs.

??? note "`DynamicApplier.restMapping` (unexported)"

    ```go
    func (a *DynamicApplier) restMapping(
        ctx context.Context, mapper meta.ResettableRESTMapper, obj *unstructured.Unstructured,
    ) (*meta.RESTMapping, error)
    ```

    - Resolves `obj`'s `GroupVersionKind` to a REST mapping, retrying only on
      `meta.IsNoMatchError` (resetting the cached discovery document before
      each retry) until `crdTimeout` elapses.
    - Exists because `HelmInstaller.Install` returns once Argo CD's chart
      (including its CRDs) is submitted, not once the API server has
      established the new `Application` type — resolving the mapping
      immediately intermittently failed with "no matches for kind
      Application" on fresh clusters.

## install.go

#### `Installer`

??? abstract "`Installer`"

    Installs or upgrades Argo CD itself into a cluster — the one piece of the
    addon pipeline that must exist before app-of-apps can sync anything, so
    it is not delivered as an Argo CD `Application` like every other addon.

    ```go
    type Installer interface {
        Install(ctx context.Context, restConfig *rest.Config, addon core.AddonRef) error
    }
    ```

    Invariants:

    - Must be safe to call on every `apply` — a no-change call must not
      error.
    - Returns only once Argo CD is actually running (not merely once
      manifests are submitted), because the caller's next act — applying the
      root `Application` — depends on the CRD this install created.

#### `HelmInstaller`

??? abstract "`HelmInstaller`"

    The real `Installer`, built on `helm.sh/helm/v3/pkg/action` rather than
    shelling out to the `helm` binary.

    ```go
    type HelmInstaller struct {
        // unexported: logger, timeout
    }

    func NewHelmInstaller(logger *slog.Logger) *HelmInstaller
    func (h *HelmInstaller) Install(ctx context.Context, restConfig *rest.Config, addon core.AddonRef) error
    ```

    - `Install` checks `releaseExists` for the fixed release name
      `ReleaseName = "argocd"`; routes to `action.NewUpgrade` if a release
      history exists, otherwise `action.NewInstall` with
      `CreateNamespace: true`.
    - Both paths set `Wait: true` and `Atomic: false`: `Wait` so "installed
      argocd" means Argo CD is actually serving (not just that manifests
      were submitted — the root `Application` applied moments later needs
      the CRDs this release creates); not `Atomic` because a rollback would
      uninstall a part-working release, and since the phase isn't recorded
      on failure anyway, a retry re-enters and converges via the upgrade
      branch — the same create-or-update, never-delete discipline as
      `internal/fleetinfra`.
    - `InstallTimeout = 10 * time.Minute` is the default wait bound
      (`timeout` field overrides it per instance, e.g. for
      slow/quota-constrained clusters).
    - Has no live-cluster test coverage — `action.Install.Run`/`Upgrade.Run`
      require a reachable API server. What's covered: the release-exists
      branch and the chart reference construction, both pure functions of
      their inputs.

??? abstract "`staticRESTClientGetter` (unexported)"

    Adapts an already-resolved `*rest.Config` to Helm's
    `genericclioptions.RESTClientGetter` interface, so Helm never needs to
    know the config came from a cloud-native token mint
    (`internal/provisioner`) rather than a kubeconfig file on disk.

    - Implements `ToRESTConfig`, `ToDiscoveryClient`, `ToRESTMapper`.
    - `ToRawKubeConfigLoader` returns an empty loader (unused by the
      Install/Upgrade/History calls this file makes, but returns a clear
      "no such context" error rather than panicking if that ever changes).

#### `DefaultAddon`

??? abstract "`DefaultAddon`"

    ```go
    var DefaultAddon = core.AddonRef{
        Name:       "argocd",
        Chart:      "argo-cd",
        Repository: "https://argoproj.github.io/argo-helm",
        Version:    "7.7.11",
        Namespace:  installNamespace,
    }
    ```

    - Used by callers when a cluster's resolved profile carries no
      `"argocd"` catalog entry of its own (true of `tier-small` today — the
      catalog only tracks Argo CD's version starting at `tier-standard`).
    - Argo CD still must be installed on every tier: app-of-apps cannot sync
      into a cluster that doesn't have it yet.

??? note "`releaseExists` (unexported)"

    ```go
    func releaseExists(cfg *action.Configuration, releaseName string) (bool, error)
    ```

    - Reports whether `releaseName` already has Helm release history via
      `action.NewHistory(cfg).Run`, so `Install` can route to the upgrade
      path rather than a fresh install.
    - Treats `driver.ErrReleaseNotFound` as `false, nil`; any other error is
      wrapped and returned.

??? note "`HelmInstaller.actionConfig` (unexported)"

    ```go
    func (h *HelmInstaller) actionConfig(restConfig *rest.Config) (*action.Configuration, error)
    ```

    - Builds a Helm `action.Configuration` addressed at `restConfig` via
      `staticRESTClientGetter`, storing release state as Secrets in
      `installNamespace` — the same storage driver (`"secret"`) `helm`
      itself defaults to.

## manifest.go

#### `Application`

??? abstract "`Application`, `ApplicationMetadata`, `ApplicationSpec`, `ApplicationSource`, `ApplicationSourceHelm`, `ApplicationDestination`, `ApplicationSyncPolicy`, `ApplicationSyncPolicyAutomated`"

    A local, minimal mirror of the subset of the Argo CD `Application` CRD
    (`argoproj.io/v1alpha1`) this package writes — not a dependency on Argo
    CD's own Go module, since kubespin only ever writes these fields, never
    reads or reconciles them.

    ```go
    type Application struct {
        APIVersion string
        Kind       string
        Metadata   ApplicationMetadata
        Spec       ApplicationSpec
    }

    type ApplicationMetadata struct {
        Name       string
        Namespace  string
        Finalizers []string // e.g. "resources-finalizer.argocd.argoproj.io"
    }

    type ApplicationSpec struct {
        Project     string
        Source      ApplicationSource
        Destination ApplicationDestination
        SyncPolicy  *ApplicationSyncPolicy
    }

    type ApplicationSource struct {
        RepoURL        string
        Path           string                 // root Application: AppsDir
        Chart          string                 // addon Applications: addon.Chart
        TargetRevision string
        Helm           *ApplicationSourceHelm
    }

    type ApplicationSourceHelm struct {
        ValuesObject map[string]any
    }

    type ApplicationDestination struct {
        Server    string // always inClusterServer
        Namespace string
    }

    type ApplicationSyncPolicy struct {
        Automated   *ApplicationSyncPolicyAutomated
        SyncOptions []string
    }

    type ApplicationSyncPolicyAutomated struct {
        Prune    bool
        SelfHeal bool
    }
    ```

    Invariants:

    - `Finalizers` always includes `resources-finalizer.argocd.argoproj.io`
      so deleting an `Application` also deletes the resources it manages
      instead of orphaning them.
    - `Destination.Server` is always `inClusterServer`
      (`https://kubernetes.default.svc`) — Argo CD is local to the cluster
      it manages, there is no central hub, so every rendered `Application`
      points at itself.
    - `SyncPolicy` is automated, pruning, and self-healing on every
      `Application` this package renders: drifted or hand-deleted resources
      converge back on their own, the same convergence discipline `apply`
      uses for cloud infra and the repo.
    - `ApplicationSourceHelm.ValuesObject` takes a structured map so an
      addon's resolved values (profile + `internal/catalog` override patch)
      pass straight through without re-serialization.

#### `Option`

??? abstract "`options` / `Option`"

    ```go
    type options struct {
        logger *slog.Logger
    }

    type Option func(*options)

    func WithLogger(logger *slog.Logger) Option
    ```

    - Carries settings accepted by the rendering entry points
      (`RenderAddonApplications`, `ApplyProfileIngressDefaults`).
    - A struct rather than a field on a type because everything in this
      package outside `install.go`/`apply.go` is a pure function over a
      `Profile` — there is no installer object to hang a logger off.
    - `resolve(opts []Option) options` applies `opts` over the default
      (`slog.Default()`).

## Package-level constants

Spread across `manifest.go`, `appofapps.go`, `install.go`, `repocreds.go`:

| Constant | File | Value | Meaning |
|---|---|---|---|
| `Namespace` | manifest.go | `"argocd"` | Where Argo CD and every `Application` it manages live; shared between `appofapps.go`'s rendering and `install.go`'s install namespace so they never drift apart. |
| `inClusterServer` | manifest.go | `"https://kubernetes.default.svc"` | Destination every rendered `Application` targets. |
| `AppsDir` | appofapps.go | `"apps"` | Directory in the cluster's own repository under which per-addon `Application` manifests are committed. |
| `ReleaseName` | install.go | `"argocd"` | Fixed Helm release name — one Argo CD install per cluster, nothing per-cluster to parameterize beyond version. |
| `InstallTimeout` | install.go | `10 * time.Minute` | Default wait bound for `HelmInstaller.Install`. |
| `repoCredsSecretName` | repocreds.go | `"repo-creds"` | Fixed name for the repo-credentials Secret — stable so re-applying on every `apply` converges instead of accumulating one Secret per run. |

## appofapps.go

#### `RenderRootApplication`

??? note "`RenderRootApplication(repoURL string) ([]byte, error)`"

    - Renders the app-of-apps root `Application` — the one resource
      installed directly into the cluster via `KubeApplier`, never
      committed to the repo it manages (an `Application` that synced itself
      would be a cycle).
    - Points its `Source` at `Path: AppsDir`, `TargetRevision: "HEAD"` of
      `repoURL`, so it discovers every manifest committed under `apps/` in
      the cluster's own repository.
    - Returns the YAML-marshaled manifest.

#### `RenderAddonApplication`

??? note "`RenderAddonApplication(addon core.AddonRef) ([]byte, error)`"

    - Renders one addon's independent `Application` (`Source.Chart`/
      `Repository`/`Version` from `addon`, values passed through
      `ApplicationSourceHelm.ValuesObject`, `SyncOptions:
      []string{"CreateNamespace=true"}`).
    - Each addon syncs and fails on its own, which is the entire point of
      app-of-apps over one monolithic `Application` for the whole profile.

#### `RenderAddonApplications`

??? note "`RenderAddonApplications(profile core.Profile, opts ...Option) (map[string][]byte, error)`"

    - Renders every addon in `profile` via `RenderAddonApplication`, keyed
      by the path each should be committed to under `AppsDir`
      (`"apps/<addon.Name>.yaml"`) in the cluster's own repository.
    - Logs one debug line per addon and a summary info line.

## repocreds.go

#### `RenderRepoCredentialsSecret`

??? note "`RenderRepoCredentialsSecret(repoURL, username, password string) ([]byte, error)`"

    - Renders the repository-credential Secret Argo CD's repo-server needs
      to clone the cluster's own repository.
    - Fixed name `repo-creds`, namespace `Namespace`, label
      `argocd.argoproj.io/secret-type: repository`, `stringData` holding
      `type: git`, `url`, `username`, `password`.
    - Because the repository is always created private, applying this
      Secret alongside the root `Application` (both via
      `KubeApplier.Apply`, never committed to git — a Secret holding a live
      token has no business in git history) is what prevents the root
      `Application`'s first reconcile from failing with "authentication
      required".

## ingress.go

#### `Exposure`

??? abstract "`Exposure`"

    ```go
    type Exposure string

    const (
        ExposureInternal Exposure = "internal"
        ExposureExternal Exposure = "external"
    )
    ```

    - How an ingress or Gateway API addon's load balancer is reachable.
    - Internal is the default in every case but one — see `ResolveExposure`.

#### `ResolveExposure`

??? note "`ResolveExposure(access core.Access, requested Exposure) Exposure`"

    ```go
    func ResolveExposure(access core.Access, requested Exposure) Exposure {
        if access == core.AccessPublic && requested == ExposureExternal {
            return ExposureExternal
        }
        return ExposureInternal
    }
    ```

    - Applies the public/private-aware ingress default: internal load
      balancer unless the cluster is `core.AccessPublic` *and* the addon
      itself asks to be external.
    - A private cluster overrides any addon-level request for external
      exposure — there is no public endpoint for an externally exposed load
      balancer to sit in front of.

??? note "`requestedExposure(values map[string]any) Exposure` (unexported)"

    - Reads an addon's own `ingress.exposure` value out of its resolved
      Helm values, defaulting to `ExposureInternal` when the addon sets
      none or the value isn't shaped as expected.

#### `ApplyIngressDefaults`

??? note "`ApplyIngressDefaults(access core.Access, addon core.AddonRef) core.AddonRef`"

    - Overlays the resolved exposure onto an ingress/Gateway addon's
      values, returning a new `AddonRef` (addon's own `Values` map is never
      mutated, so a caller iterating a profile's addons cannot leak one
      addon's ingress defaults into another).
    - Always writes an explicit `ingress.exposure` string and an
      `ingress.internal` bool (`exposure == ExposureInternal`) that chart
      authors can key an annotation off — proving the access-mode default
      was applied rather than trusting the profile got it right.

#### `ApplyProfileIngressDefaults`

??? note "`ApplyProfileIngressDefaults(access core.Access, profile core.Profile, opts ...Option) core.Profile`"

    - Applies `ApplyIngressDefaults` to every addon in `profile` that
      already declares an `ingress` values block, leaving every other addon
      untouched (an addon with no opinion about ingress should not gain an
      empty `ingress: {}` block just because some other addon in the
      profile is a load balancer).
    - Logs a warning via the resolved `Option`'s logger when an addon
      requested `ExposureExternal` but `ResolveExposure` forced it back to
      internal.
