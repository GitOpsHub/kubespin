# internal/catalog

Profile resolution turns a `core.ProfileRef` (e.g. `tier-small@1.0.0`) into the
addon set a cluster's `addons.yaml` renders. "Resolution" here means: look up
the named tier's base `core.Profile`, drop addons the cluster's cloud doesn't
support (`core.Profile.ForProvider`), then apply the cluster's per-cluster
override patch on top — `Merge` patches addons *in place* (version bump,
one-level value overlay, or drop) rather than adding or duplicating entries,
so the resolved profile never diverges structurally from the catalog it came
from. `internal/orchestrator.resolveProfile` (`internal/orchestrator/steps.go`)
drives this sequence — `Resolve` → `ForProvider` → `Merge` — and hands the
result to `argocd.ApplyProfileIngressDefaults` for access-mode templating
before `internal/repo.ReconcileAppOfApps` commits it as the cluster repo's
`addons.yaml`.

## Types

### `Resolver`

```go
type Resolver interface {
	Resolve(ctx context.Context, ref core.ProfileRef) (core.Profile, error)
}
```

*Source: `catalog.go`*

The seam profile resolution happens behind. `internal/orchestrator` and the
rest of upstream code depend only on this interface, so swapping the backing
store (builtin map today, `platform-profiles` repo under M4) requires no
change above this package.

### `BuiltinResolver`

```go
type BuiltinResolver struct {
	profiles map[string]core.Profile
}

func NewBuiltinResolver() *BuiltinResolver
func (r *BuiltinResolver) Resolve(_ context.Context, ref core.ProfileRef) (core.Profile, error)
```

*Source: `catalog.go`*

Serves a fixed, in-memory set of the three builtin tiers, keyed by
`ProfileRef.String()` (`"<name>@<version>"`), so two versions of the same
profile name are distinct entries — required for `fleet update` to reason
about pinned vs. target versions. `Resolve` returns `ErrProfileNotFound`
wrapped with the requested ref when no entry matches. This is a placeholder
for the real `platform-profiles`-repo-backed catalog that Milestone 4
introduces (`RepoResolver`); it exists so the repo-seeding and idempotent-diff
machinery in `internal/repo` has a real profile to render against.

### `RepoResolver`

```go
type RepoResolver struct {
	files    FileReader
	repoName string
}

func NewRepoResolver(files FileReader, repoName string) *RepoResolver
func (r *RepoResolver) Resolve(ctx context.Context, ref core.ProfileRef) (core.Profile, error)
```

*Source: `repo_resolver.go`*

The Milestone 4 resolver: reads profile definitions from a
`platform-profiles`-style GitHub repository, one YAML file per `(name,
version)` pair at `profiles/<name>/<version>.yaml`. `Resolve`:

1. Validates `ref` (`ProfileRef.Validate`).
2. Reads `profiles/<name>/<version>.yaml` from `repoName` via `files`.
3. Returns `ErrProfileNotFound` if the file does not exist.
4. Unmarshals YAML into `core.Profile` and validates it (`Profile.Validate`).
5. Errors if the parsed profile's own `Ref()` does not match the requested
   `ref` — a profile file's declared name/version must agree with the path it
   was read from.

**Invariant:** `RepoResolver` never talks to GitHub directly — it depends only
on `FileReader`, so this package needs no knowledge of GitHub or `internal/repo`'s
fakes to be tested.

### `FileReader`

```go
type FileReader interface {
	ReadFile(ctx context.Context, repoName, path string) ([]byte, bool, error)
}
```

*Source: `repo_resolver.go`*

The read seam `RepoResolver` depends on. `*repo.Clients` satisfies it in
production; the returned `bool` is a found/not-found flag distinct from a
transport error.

## Functions

### `Merge`

```go
func Merge(profile core.Profile, overrides []core.AddonOverride) (core.Profile, error)
```

*Source: `merge.go`*

Applies a cluster's `[]core.AddonOverride` patch onto a resolved
`core.Profile`, returning the patched copy.

- No-op (returns `profile` unchanged) when `overrides` is empty.
- For each override, looks up the addon by `Name`. An override naming an
  addon the profile does not carry returns `ErrUnknownOverride` wrapped with
  the addon name and profile ref — a typo in a per-cluster patch must surface
  at apply time, not be silently dropped.
- `Version`, if set, replaces the addon's version.
- `Values`, if set, is overlaid onto the addon's existing values via
  `MergeValues`.
- `Disable: true` removes the addon from the merged set entirely, after all
  patches are applied.

**Invariant:** never adds a new addon and never duplicates one — every name in
the override list must already exist in `profile.Addons`. The profile's
backing `Addons` slice is copied before mutation, so the source profile passed
in is never aliased/mutated.

### `MergeValues`

```go
func MergeValues(base, override map[string]any) map[string]any
```

*Source: `merge.go`*

One-level-deep overlay of `override` onto `base`: every key in `override`
replaces the same key in `base`; keys only in `base` are kept as-is. Nested
maps are replaced wholesale, not deep-merged — going deeper would mean
guessing at merge semantics (replace vs. deep-merge a slice, for instance)
that only the addon's own chart can judge. Exported specifically so
`internal/orchestrator`'s `argoCDAddon` can apply the same one-level overlay
to `argocd.DefaultAddon`, which never appears in a profile's own `Addons` list
for `Merge` to patch in place (Argo CD is installed directly via the Helm SDK,
not through app-of-apps).

## Builtin profiles (`tiers.go`)

Three package-level `core.Profile` values, registered into `BuiltinResolver`
by `NewBuiltinResolver`. All are explicitly marked as placeholders for the
real `platform-profiles`-repo-backed catalog entries Milestone 4 introduces —
superseded, not rewritten, once that repo exists.

- **`tierSmall`** (`tier-small@1.0.0`) — the full baseline addon set named in
  the implementation plan: `cilium` (CNI), `cert-manager`, `gateway-api`,
  `external-secrets`, `cluster-autoscaler`, `kube-prometheus-stack`,
  `fluent-bit`, `opencost`, `external-dns`, `ingress-nginx`, `kyverno`,
  `kyverno-policies`, `fleet-status-reporter`. Built from real public Helm
  charts so it renders and installs as-is. `ingress-nginx` defaults
  `ingress.exposure` to `"internal"` until `internal/argocd.ApplyIngressDefaults`
  overlays the resolved access-mode value; `kyverno-policies` sets
  `policies.publicExposureDeny: true`, the baseline admission rule the
  project's architecture invariants require regardless of access mode.
- **`tierStandard`** (`tier-standard@1.0.0`) — `tierSmall`'s addons
  (via the `withAddons` helper) plus `argocd`, `velero`, `falco`, and
  `karpenter`. `argocd` is tracked here purely so `fleet audit`/`fleet update`
  can see and pin its version, even though Argo CD is installed directly
  (Helm-as-library), not through app-of-apps. `karpenter` carries
  `Providers: []core.Provider{core.ProviderAWS}` (it is EKS-specific), so
  `core.Profile.ForProvider` drops it for GCP/Azure clusters before an
  override patch or Argo CD ever sees it.
- **`tierRegulated`** (`tier-regulated@1.0.0`) — `tierStandard`'s addons with
  `kyverno-policies` *replaced* (via `replaceAddon`, not appended) by a
  stricter `kyverno-policies-regulated` chart (`publicExposureDeny`,
  `denyPrivilegedPods`, `mandatoryQuotas`, `mandatoryNetworkPolicy`,
  `requireImageSignature` all `true`), plus `audit-logging` and
  `otel-collector`. Replacing rather than layering avoids two Argo CD
  Applications installing overlapping `ClusterPolicy` resources into the same
  cluster and fighting over ownership.

### `withAddons`

```go
func withAddons(base []core.AddonRef, extra ...core.AddonRef) []core.AddonRef
```

*Source: `tiers.go`*

Returns a copy of `base` with `extra` appended, backed by a freshly allocated
array — appending to `tierSmall.Addons` directly would risk one tier's growth
silently overwriting another's slice if their capacities ever happened to
overlap.

### `replaceAddon`

```go
func replaceAddon(addons []core.AddonRef, name string, replacement core.AddonRef) []core.AddonRef
```

*Source: `tiers.go`*

Returns a copy of `addons` with the entry named `name` swapped for
`replacement`, again without aliasing the input slice's backing array. Used
by `tierRegulated` to supersede `tierSmall`'s baseline `kyverno-policies` addon.

## Errors

```go
var ErrProfileNotFound = errors.New("profile not found")
var ErrUnknownOverride = errors.New("override does not match any addon in the profile")
```

`ErrProfileNotFound` is returned by both `BuiltinResolver.Resolve` and
`RepoResolver.Resolve` when no profile matches the requested `ProfileRef`.
`ErrUnknownOverride` is returned by `Merge` when a `core.AddonOverride.Name`
does not match any addon already in the profile.
