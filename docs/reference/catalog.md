# internal/catalog

Profile resolution turns a `core.ProfileRef` (e.g. `tier-small@1.0.0`) into the addon set a cluster's `addons.yaml` renders. "Resolution" here means: look up the named tier's base `core.Profile`, drop addons the cluster's cloud doesn't support (`core.Profile.ForProvider`), then apply the cluster's per-cluster override patch on top — `Merge` patches addons *in place* (version bump, one-level value overlay, or drop) rather than adding or duplicating entries, so the resolved profile never diverges structurally from the catalog it came from. `ResolveForCluster` (`resolve.go`) drives this whole sequence — `Resolve` → `ForProvider` → inject a stand-in `argocd` addon → `Merge` → `argocd.ApplyProfileIngressDefaults` for access-mode templating — and is the single seam both `internal/orchestrator` (apply) and `internal/fleet` (`fleet update`) call, so the two can never resolve the same cluster's profile differently.

## Quick reference

| Name | Kind | File | Summary |
|---|---|---|---|
| [`ResolveForCluster`](#resolveforcluster) | function | `resolve.go` | Full resolve → provider-filter → argocd-stand-in → merge → ingress-template sequence for one cluster |
| [`Resolver`](#resolver) | interface | `catalog.go` | Seam profile resolution happens behind |
| [`BuiltinResolver`](#builtinresolver) | type | `catalog.go` | Fixed, in-memory resolver over the three builtin tiers |
| [`RepoResolver`](#reporesolver) | type | `repo_resolver.go` | Milestone 4 resolver reading profiles from a `platform-profiles`-style repo |
| [`FileReader`](#filereader) | interface | `repo_resolver.go` | Read seam `RepoResolver` depends on |
| [`Merge`](#merge) | function | `merge.go` | Applies a cluster's override patch onto a resolved profile |
| [`mergeValues`](#mergevalues) | function | `merge.go` | One-level-deep overlay of override values onto base values (unexported) |
| [`withAddons`](#withaddons) | function | `tiers.go` | Returns a copy of a base addon list plus extras, without aliasing |
| [`replaceAddon`](#replaceaddon) | function | `tiers.go` | Returns a copy of an addon list with one entry swapped out |
| [`ErrProfileNotFound`](#errprofilenotfound) | sentinel error | `catalog.go` | No profile matches the requested `ProfileRef` |
| [`ErrUnknownOverride`](#errunknownoverride) | sentinel error | `merge.go` | An override names an addon the profile does not carry |

## `resolve.go`

#### `ResolveForCluster`

??? note "`ResolveForCluster` — function"

    ```go
    func ResolveForCluster(ctx context.Context, resolver Resolver, spec core.ClusterSpec) (core.Profile, error)
    ```

    - **Behavior:** `resolver.Resolve(ctx, spec.Profile)`, then `Profile.ForProvider(spec.Provider)` to drop unsupported addons, then `withArgoCDAddon` (unexported: injects `argocd.DefaultAddon` as a stand-in `"argocd"` catalog entry when the profile doesn't carry one of its own — true below tier-standard), then `Merge(profile, spec.Overrides)`, then `argocd.ApplyProfileIngressDefaults(spec.Access, merged)`.
    - **Invariant:** because `withArgoCDAddon` runs before `Merge`, every resolved profile always has an `"argocd"` entry, so a `cluster.yaml` override naming `"argocd"` is always legal — the caller no longer has to special-case tiers that don't catalog Argo CD themselves.
    - **Behavior:** the single seam both `internal/orchestrator` (`installArgoCDStep`, `seedRepoStep`, `ReadyReconcile`) and `internal/fleet.UpdateOne` call, so `apply` and `fleet update` resolve the same cluster's profile identically.

## `catalog.go`

#### `Resolver`

??? abstract "`Resolver` — interface"

    ```go
    type Resolver interface {
    	Resolve(ctx context.Context, ref core.ProfileRef) (core.Profile, error)
    }
    ```

    - **Behavior:** the seam profile resolution happens behind. `internal/orchestrator` and the rest of upstream code depend only on this interface, so swapping the backing store (builtin map today, `platform-profiles` repo under M4) requires no change above this package.

#### `BuiltinResolver`

??? abstract "`BuiltinResolver` — type"

    ```go
    type BuiltinResolver struct {
    	profiles map[string]core.Profile
    }

    func NewBuiltinResolver() *BuiltinResolver
    func (r *BuiltinResolver) Resolve(_ context.Context, ref core.ProfileRef) (core.Profile, error)
    ```

    - **Behavior:** serves a fixed, in-memory set of the three builtin tiers, keyed by `ProfileRef.String()` (`"<name>@<version>"`), so two versions of the same profile name are distinct entries — required for `fleet update` to reason about pinned vs. target versions.
    - **Behavior:** `Resolve` returns `ErrProfileNotFound` wrapped with the requested ref when no entry matches.
    - **Invariant:** this is a placeholder for the real `platform-profiles`-repo-backed catalog that Milestone 4 introduces (`RepoResolver`); it exists so the repo-seeding and idempotent-diff machinery in `internal/repo` has a real profile to render against.

#### `ErrProfileNotFound`

??? abstract "`ErrProfileNotFound` — sentinel error"

    ```go
    var ErrProfileNotFound = errors.New("profile not found")
    ```

    - **Behavior:** returned by both `BuiltinResolver.Resolve` and `RepoResolver.Resolve` when no profile matches the requested `ProfileRef`.

## `repo_resolver.go`

#### `RepoResolver`

??? abstract "`RepoResolver` — type"

    ```go
    type RepoResolver struct {
    	files    FileReader
    	repoName string
    }

    func NewRepoResolver(files FileReader, repoName string) *RepoResolver
    func (r *RepoResolver) Resolve(ctx context.Context, ref core.ProfileRef) (core.Profile, error)
    ```

    The Milestone 4 resolver: reads profile definitions from a `platform-profiles`-style GitHub repository, one YAML file per `(name, version)` pair at `profiles/<name>/<version>.yaml`.

    - **Behavior:** `Resolve`:
        1. Validates `ref` (`ProfileRef.Validate`).
        2. Reads `profiles/<name>/<version>.yaml` from `repoName` via `files`.
        3. Returns `ErrProfileNotFound` if the file does not exist.
        4. Unmarshals YAML into `core.Profile` and validates it (`Profile.Validate`).
        5. Errors if the parsed profile's own `Ref()` does not match the requested `ref` — a profile file's declared name/version must agree with the path it was read from.
    - **Invariant:** `RepoResolver` never talks to GitHub directly — it depends only on `FileReader`, so this package needs no knowledge of GitHub or `internal/repo`'s fakes to be tested.

#### `FileReader`

??? abstract "`FileReader` — interface"

    ```go
    type FileReader interface {
    	ReadFile(ctx context.Context, repoName, path string) ([]byte, bool, error)
    }
    ```

    - **Behavior:** the read seam `RepoResolver` depends on. `*repo.Clients` satisfies it in production; the returned `bool` is a found/not-found flag distinct from a transport error.

## `merge.go`

#### `Merge`

??? note "`Merge` — function"

    ```go
    func Merge(profile core.Profile, overrides []core.AddonOverride) (core.Profile, error)
    ```

    Applies a cluster's `[]core.AddonOverride` patch onto a resolved `core.Profile`, returning the patched copy.

    - **Behavior:** no-op (returns `profile` unchanged) when `overrides` is empty.
    - **Behavior:** for each override, looks up the addon by `Name`. An override naming an addon the profile does not carry returns `ErrUnknownOverride` wrapped with the addon name and profile ref — a typo in a per-cluster patch must surface at apply time, not be silently dropped.
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
    - **Behavior:** unexported — `Merge` (this file) is now its only caller since `ResolveForCluster`'s argocd stand-in patches the profile's `Addons` before `Merge` runs, rather than applying an overlay directly to `argocd.DefaultAddon` from outside the package.

#### `ErrUnknownOverride`

??? abstract "`ErrUnknownOverride` — sentinel error"

    ```go
    var ErrUnknownOverride = errors.New("override does not match any addon in the profile")
    ```

    - **Behavior:** returned by `Merge` when a `core.AddonOverride.Name` does not match any addon already in the profile.

## `tiers.go`

Three package-level `core.Profile` values, registered into `BuiltinResolver` by `NewBuiltinResolver`. All are explicitly marked as placeholders for the real `platform-profiles`-repo-backed catalog entries Milestone 4 introduces — superseded, not rewritten, once that repo exists.

| Tier | Ref | Addons |
|---|---|---|
| `tierSmall` | `tier-small@1.0.0` | The full baseline addon set named in the implementation plan: `cilium` (CNI), `cert-manager`, `gateway-api`, `external-secrets`, `cluster-autoscaler`, `kube-prometheus-stack`, `fluent-bit`, `opencost`, `external-dns`, `ingress-nginx`, `kyverno`, `kyverno-policies`, `fleet-status-reporter`. Built from real public Helm charts so it renders and installs as-is. `ingress-nginx` defaults `ingress.exposure` to `"internal"` until `internal/argocd.ApplyIngressDefaults` overlays the resolved access-mode value; `kyverno-policies` sets `policies.publicExposureDeny: true`, the baseline admission rule the project's architecture invariants require regardless of access mode. |
| `tierStandard` | `tier-standard@1.0.0` | `tierSmall`'s addons (via the `withAddons` helper) plus `argocd`, `velero`, `falco`, and `karpenter`. `argocd` is tracked here purely so `fleet audit`/`fleet update` can see and pin its version, even though Argo CD is installed directly (Helm-as-library), not through app-of-apps. `karpenter` carries `Providers: []core.Provider{core.ProviderAWS}` (it is EKS-specific), so `core.Profile.ForProvider` drops it for GCP/Azure clusters before an override patch or Argo CD ever sees it. |
| `tierRegulated` | `tier-regulated@1.0.0` | `tierStandard`'s addons with `kyverno-policies` *replaced* (via `replaceAddon`, not appended) by a stricter `kyverno-policies-regulated` chart (`publicExposureDeny`, `denyPrivilegedPods`, `mandatoryQuotas`, `mandatoryNetworkPolicy`, `requireImageSignature` all `true`), plus `audit-logging` and `otel-collector`. Replacing rather than layering avoids two Argo CD Applications installing overlapping `ClusterPolicy` resources into the same cluster and fighting over ownership. |

#### `withAddons`

??? note "`withAddons` — function"

    ```go
    func withAddons(base []core.AddonRef, extra ...core.AddonRef) []core.AddonRef
    ```

    - **Behavior:** returns a copy of `base` with `extra` appended, backed by a freshly allocated array — appending to `tierSmall.Addons` directly would risk one tier's growth silently overwriting another's slice if their capacities ever happened to overlap.

#### `replaceAddon`

??? note "`replaceAddon` — function"

    ```go
    func replaceAddon(addons []core.AddonRef, name string, replacement core.AddonRef) []core.AddonRef
    ```

    - **Behavior:** returns a copy of `addons` with the entry named `name` swapped for `replacement`, again without aliasing the input slice's backing array. Used by `tierRegulated` to supersede `tierSmall`'s baseline `kyverno-policies` addon.
