# internal/catalog

Size resolution turns a `core.ClusterSize` (`small`, `medium`, or `large`)
into the addon set a cluster's `addons.yaml` renders. "Resolution" here
means: look up the size's base `core.Profile`, drop addons the cluster's
cloud doesn't support (`core.Profile.ForProvider`), then apply the cluster's
per-cluster override patch on top — `Merge` patches addons *in place*
(version bump, one-level value overlay, or drop) rather than adding or
duplicating entries, so the resolved profile never diverges structurally
from the catalog it came from. `ResolveForCluster` (`resolve.go`) drives
this whole sequence — `Resolve` → `ForProvider` → defensive argocd-stand-in
→ `Merge` → `argocd.ApplyProfileIngressDefaults` for access-mode templating
— and is the single seam both `internal/orchestrator` (apply) and
`internal/fleet` (`fleet update`) call, so the two can never resolve the
same cluster's size differently.

There is no external profiles repository. Every size is fully defined in
this package's Go source — `BuiltinResolver` is the only `Resolver`
implementation. Changing what a size includes means shipping a new kubespin
build, not editing an external repo or pinning a version.

## Quick reference

| Name | Kind | File | Summary |
|---|---|---|---|
| [`ResolveForCluster`](#resolveforcluster) | function | `resolve.go` | Full resolve → provider-filter → argocd-stand-in → merge → ingress-template sequence for one cluster |
| [`Resolver`](#resolver) | interface | `catalog.go` | Seam size resolution happens behind |
| [`BuiltinResolver`](#builtinresolver) | type | `catalog.go` | Fixed, in-memory resolver over the three builtin sizes |
| [`baseAddons`](#baseaddons) | var | `catalog.go` | Addon set every size carries: CNI, cert-manager, Gateway API, ESO, Kyverno baseline, monitoring/logging/cost, Argo CD, and a cloud-appropriate autoscaler |
| [`Merge`](#merge) | function | `merge.go` | Applies a cluster's override patch onto a resolved profile |
| [`mergeValues`](#mergevalues) | function | `merge.go` | One-level-deep overlay of override values onto base values (unexported) |
| [`withAddons`](#withaddons) | function | `tiers.go` | Returns a copy of a base addon list plus extras, without aliasing |
| [`replaceAddon`](#replaceaddon) | function | `tiers.go` | Returns a copy of an addon list with one entry swapped out |
| [`ErrProfileNotFound`](#errprofilenotfound) | sentinel error | `catalog.go` | No profile matches the requested `ClusterSize` |
| [`ErrUnknownOverride`](#errunknownoverride) | sentinel error | `merge.go` | An override names an addon the profile does not carry |

## `resolve.go`

#### `ResolveForCluster`

??? note "`ResolveForCluster` — function"

    ```go
    func ResolveForCluster(ctx context.Context, resolver Resolver, spec core.ClusterSpec) (core.Profile, error)
    ```

    - **Behavior:** `resolver.Resolve(ctx, spec.Size)`, then `Profile.ForProvider(spec.Provider)` to drop unsupported addons, then `withArgoCDAddon` (unexported: injects `argocd.DefaultAddon` as a defensive stand-in `"argocd"` catalog entry on the rare chance a size's catalog entry doesn't carry one — every builtin size does, via `baseAddons`), then `Merge(profile, spec.Overrides)`, then `argocd.ApplyProfileIngressDefaults(spec.Access, merged)`.
    - **Invariant:** because `withArgoCDAddon` runs before `Merge`, every resolved profile always has an `"argocd"` entry, so a `cluster.yaml` override naming `"argocd"` is always legal.
    - **Behavior:** the single seam both `internal/orchestrator` (`installArgoCDStep`, `seedRepoStep`, `ReadyReconcile`) and `internal/fleet.UpdateOne` call, so `apply` and `fleet update` resolve the same cluster's size identically.

## `catalog.go`

#### `Resolver`

??? abstract "`Resolver` — interface"

    ```go
    type Resolver interface {
    	Resolve(ctx context.Context, size core.ClusterSize) (core.Profile, error)
    }
    ```

    - **Behavior:** the seam size resolution happens behind. `internal/orchestrator` and the rest of upstream code depend only on this interface — today `BuiltinResolver` is the only implementation.

#### `BuiltinResolver`

??? abstract "`BuiltinResolver` — type"

    ```go
    type BuiltinResolver struct {
    	profiles map[core.ClusterSize]core.Profile
    }

    func NewBuiltinResolver() *BuiltinResolver
    func (r *BuiltinResolver) Resolve(_ context.Context, size core.ClusterSize) (core.Profile, error)
    ```

    - **Behavior:** serves a fixed, in-memory map of the three builtin sizes (`sizeSmall`, `sizeMedium`, `sizeLarge` from `tiers.go`), keyed by `core.ClusterSize`.
    - **Behavior:** `Resolve` returns `ErrProfileNotFound` wrapped with the requested size when no entry matches (e.g. an unrecognized string cast to `core.ClusterSize`).

#### `baseAddons`

??? note "`baseAddons` — var"

    ```go
    var baseAddons = []core.AddonRef{ /* cilium, cert-manager, gateway-api,
    	external-secrets, cluster-autoscaler, karpenter, kube-prometheus-stack,
    	fluent-bit, opencost, external-dns, ingress-nginx, kyverno,
    	kyverno-policies, fleet-status-reporter, argocd.DefaultAddon */ }
    ```

    - **Behavior:** the addon set every size carries, unconditionally — this is what "Argo CD and an autoscaler ship at every size" means in practice. `sizeSmall` is exactly this list; `sizeMedium`/`sizeLarge` layer on top of it via `withAddons`/`replaceAddon`.
    - **Invariant — autoscaler mutual exclusion:** `cluster-autoscaler` carries `Providers: []core.Provider{core.ProviderGCP, core.ProviderAzure}`; `karpenter` carries `Providers: []core.Provider{core.ProviderAWS}`. `core.Profile.ForProvider` drops whichever doesn't apply before an override patch or Argo CD ever sees it, so a given cluster only ever renders one of the two. Karpenter is genuinely EKS-only technology — there is no GCP/Azure port — so `cluster-autoscaler` is the functional equivalent on those clouds, not a lesser fallback.
    - `ingress-nginx` defaults `ingress.exposure` to `"internal"` until `internal/argocd.ApplyIngressDefaults` overlays the resolved access-mode value; `kyverno-policies` sets `policies.publicExposureDeny: true`, the baseline admission rule the project's architecture invariants require regardless of access mode.

#### `ErrProfileNotFound`

??? abstract "`ErrProfileNotFound` — sentinel error"

    ```go
    var ErrProfileNotFound = errors.New("profile not found")
    ```

    - **Behavior:** returned by `BuiltinResolver.Resolve` when no profile matches the requested `core.ClusterSize`.

## `merge.go`

#### `Merge`

??? note "`Merge` — function"

    ```go
    func Merge(profile core.Profile, overrides []core.AddonOverride) (core.Profile, error)
    ```

    Applies a cluster's `[]core.AddonOverride` patch onto a resolved `core.Profile`, returning the patched copy. This is the mechanism a cluster uses to customize its addon set beyond what its size includes — see [Per-cluster override patch](../examples.md#per-cluster-override-patch) for a worked `cluster.yaml` example.

    - **Behavior:** no-op (returns `profile` unchanged) when `overrides` is empty.
    - **Behavior:** for each override, looks up the addon by `Name`. An override naming an addon the profile does not carry returns `ErrUnknownOverride` wrapped with the addon name and profile name — a typo in a per-cluster patch must surface at apply time, not be silently dropped.
    - **Behavior:** `Version`, if set, replaces the addon's version.
    - **Behavior:** `Values`, if set, is overlaid onto the addon's existing values via `mergeValues`.
    - **Behavior:** `Disable: true` removes the addon from the merged set entirely, after all patches are applied.
    - **Invariant:** never adds a new addon and never duplicates one — every name in the override list must already exist in `profile.Addons`. The profile's backing `Addons` slice is copied before mutation, so the source profile passed in is never aliased/mutated.

#### `mergeValues`

??? note "`mergeValues` — function"

    ```go
    func mergeValues(base, override map[string]any) map[string]any
    ```

    - **Behavior:** one-level-deep overlay of `override` onto `base`: every key in `override` replaces the same key in `base`; keys only in `base` are kept as-is. Nested maps are replaced wholesale, not deep-merged — going deeper would mean guessing at merge semantics (replace vs. deep-merge a slice, for instance) that only the addon's own chart can judge.

#### `ErrUnknownOverride`

??? abstract "`ErrUnknownOverride` — sentinel error"

    ```go
    var ErrUnknownOverride = errors.New("override does not match any addon in the profile")
    ```

    - **Behavior:** returned by `Merge` when a `core.AddonOverride.Name` does not match any addon already in the profile.

## `tiers.go`

Three package-level `core.Profile` values, registered into `BuiltinResolver`
by `NewBuiltinResolver`, each layering onto `catalog.go`'s `baseAddons`:

| Size | `Profile.Name` | Addons |
|---|---|---|
| `sizeSmall` | `"small"` | Exactly `baseAddons` — no layer on top. |
| `sizeMedium` | `"medium"` | `baseAddons` (via `withAddons`) plus `velero` and `falco`. |
| `sizeLarge` | `"large"` | `sizeMedium`'s addons with `kyverno-policies` *replaced* (via `replaceAddon`, not appended) by a stricter `kyverno-policies-regulated` chart (`publicExposureDeny`, `denyPrivilegedPods`, `mandatoryQuotas`, `mandatoryNetworkPolicy`, `requireImageSignature` all `true`), plus `audit-logging` and `otel-collector`. |

**What each size adds, concretely** (the size-comparison a reader most often
wants):

- **small → medium**: `+velero`, `+falco`. Everything else — including Argo
  CD and the autoscaler — is already present at `small`; medium is a strict
  superset.
- **medium → large**: `kyverno-policies` is *swapped*, not added to — the
  baseline `kyverno-policies-baseline` chart (just `publicExposureDeny`) is
  replaced by `kyverno-policies-regulated` (five strict rules). Replacing
  rather than layering avoids two Argo CD Applications installing
  overlapping `ClusterPolicy` resources into the same cluster and fighting
  over ownership. Plus `+audit-logging`, `+otel-collector`.

#### `withAddons`

??? note "`withAddons` — function"

    ```go
    func withAddons(base []core.AddonRef, extra ...core.AddonRef) []core.AddonRef
    ```

    - **Behavior:** returns a copy of `base` with `extra` appended, backed by a freshly allocated array — appending to `baseAddons` directly would risk one size's growth silently overwriting another's slice if their capacities ever happened to overlap.

#### `replaceAddon`

??? note "`replaceAddon` — function"

    ```go
    func replaceAddon(addons []core.AddonRef, name string, replacement core.AddonRef) []core.AddonRef
    ```

    - **Behavior:** returns a copy of `addons` with the entry named `name` swapped for `replacement`, again without aliasing the input slice's backing array. Used by `sizeLarge` to supersede `sizeMedium`'s baseline `kyverno-policies` addon.
