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
— and is the single seam every caller goes through, so a given cluster's
size always resolves the same way.

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

<details>
<summary>`ResolveForCluster` — function</summary>

```go
func ResolveForCluster(ctx context.Context, resolver Resolver, spec core.ClusterSpec) (core.Profile, error)
```

- **Behavior:** `resolver.Resolve(ctx, spec.Size)`, then `Profile.ForProvider(spec.Provider)` to drop unsupported addons, then `withArgoCDAddon` (unexported: injects `argocd.DefaultAddon` as a defensive stand-in `"argocd"` catalog entry on the rare chance a size's catalog entry doesn't carry one — every builtin size does, via `baseAddons`), then `Merge(profile, spec.Overrides)`, then `argocd.ApplyProfileIngressDefaults(spec.Access, merged, argocd.WithAuthorizedCIDRs(spec.AuthorizedCIDRs))`.
- **Invariant:** because `withArgoCDAddon` runs before `Merge`, every resolved profile always has an `"argocd"` entry, so a `cluster.yaml` override naming `"argocd"` is always legal.
- **Behavior:** the single seam `internal/orchestrator` goes through (`installArgoCDStep`, `seedRepoStep`, `ReadyReconcile`), so every step of an `apply` resolves the same cluster's size identically.

</details>

## `catalog.go`

#### `Resolver`

<details>
<summary>`Resolver` — interface</summary>

```go
type Resolver interface {
	Resolve(ctx context.Context, size core.ClusterSize) (core.Profile, error)
}
```

- **Behavior:** the seam size resolution happens behind. `internal/orchestrator` and the rest of upstream code depend only on this interface — today `BuiltinResolver` is the only implementation.

</details>

#### `BuiltinResolver`

<details>
<summary>`BuiltinResolver` — type</summary>

```go
type BuiltinResolver struct {
	profiles map[core.ClusterSize]core.Profile
}

func NewBuiltinResolver() *BuiltinResolver
func (r *BuiltinResolver) Resolve(_ context.Context, size core.ClusterSize) (core.Profile, error)
```

- **Behavior:** serves a fixed, in-memory map of the three builtin sizes (`sizeSmall`, `sizeMedium`, `sizeLarge` from `tiers.go`), keyed by `core.ClusterSize`.
- **Behavior:** `Resolve` returns `ErrProfileNotFound` wrapped with the requested size when no entry matches (e.g. an unrecognized string cast to `core.ClusterSize`).

</details>

#### `baseAddons`

<details>
<summary>`baseAddons` — var</summary>

```go
var baseAddons = []core.AddonRef{ /* cert-manager, gateway-api,
	external-secrets, cluster-autoscaler (aws), kube-prometheus-stack,
	fluent-bit, external-dns, ingress-nginx, kyverno,
	kyverno-policies, argocd.DefaultAddon */ }
```

- **Behavior:** the addon set every size carries, unconditionally — this is what "Argo CD and an autoscaler ship at every size" means in practice. `sizeSmall` is exactly this list; `sizeMedium`/`sizeLarge` layer on top of it via `withAddons`/`replaceAddon`.
- **Autoscaling — AWS chart, native elsewhere:** `cluster-autoscaler` is catalogued for AWS only (`Providers: []core.Provider{core.ProviderAWS}`). GKE and AKS node pools are created with the clouds' own node-pool autoscaling enabled, so GCP and Azure clusters carry no autoscaler chart; an earlier GCP/Azure entry shipped with no values, so the chart defaulted to `cloudProvider: aws` and could never work there. The AWS entry's values use `${CLUSTER_ID}` and `${REGION}` placeholders, which `ResolveForCluster` fills in before overrides apply. It finds the EKS managed node groups through the `k8s.io/cluster-autoscaler/<cluster>` tag EKS puts on their Auto Scaling groups, and it gets its AWS permissions through EKS Pod Identity (see `provisioner-aws.md`).
- `ingress-nginx`'s controller load balancer (`controller.service`), like argocd-server's (`server.service`), is left internet-facing on every cluster, private included: operator UIs depend on it, and access-mode templating only touches a top-level `service`; `kyverno-policies` is Kyverno's upstream Pod Security Standards chart at `baseline`, with `validationFailureAction: Audit`. Violations are reported in PolicyReports but not blocked. There is no public-exposure-deny policy: the `charts.kubespin.dev` repository it came from never existed.

</details>

#### `ErrProfileNotFound`

<details>
<summary>`ErrProfileNotFound` — sentinel error</summary>

```go
var ErrProfileNotFound = errors.New("profile not found")
```

- **Behavior:** returned by `BuiltinResolver.Resolve` when no profile matches the requested `core.ClusterSize`.

</details>

## `merge.go`

#### `Merge`

<details>
<summary>`Merge` — function</summary>

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

</details>

#### `mergeValues`

<details>
<summary>`mergeValues` — function</summary>

```go
func mergeValues(base, override map[string]any) map[string]any
```

- **Behavior:** one-level-deep overlay of `override` onto `base`: every key in `override` replaces the same key in `base`; keys only in `base` are kept as-is. Nested maps are replaced wholesale, not deep-merged — going deeper would mean guessing at merge semantics (replace vs. deep-merge a slice, for instance) that only the addon's own chart can judge.

</details>

#### `ErrUnknownOverride`

<details>
<summary>`ErrUnknownOverride` — sentinel error</summary>

```go
var ErrUnknownOverride = errors.New("override does not match any addon in the profile")
```

- **Behavior:** returned by `Merge` when a `core.AddonOverride.Name` does not match any addon already in the profile.

</details>

## `tiers.go`

Three package-level `core.Profile` values, registered into `BuiltinResolver`
by `NewBuiltinResolver`, each layering onto `catalog.go`'s `baseAddons`:

| Size | `Profile.Name` | Addons |
|---|---|---|
| `sizeSmall` | `"small"` | Exactly `baseAddons` — no layer on top. |
| `sizeMedium` | `"medium"` | `baseAddons` (via `withAddons`) plus `velero` and `falco`. |
| `sizeLarge` | `"large"` | `sizeMedium`'s addons with `kyverno-policies` *replaced* (via `replaceAddon`, not appended) by the same chart at `podSecurityStandard: restricted` (still `Audit`), plus `otel-collector` (`mode: deployment`, `image.repository: otel/opentelemetry-collector-k8s`, since the chart has no default for either). |

**What each size adds, concretely** (the size-comparison a reader most often
wants):

- **small → medium**: `+velero`, `+falco`. Everything else — including Argo
  CD and the autoscaler — is already present at `small`; medium is a strict
  superset.
- **medium → large**: `kyverno-policies` is *swapped*, not added to. The
  `baseline` Pod Security level becomes `restricted`. Replacing rather than
  layering avoids two Argo CD Applications installing overlapping policies
  into the same cluster and fighting over ownership. Both stay in Audit mode:
  restricted forbids the host access node-exporter, falco and fluent-bit
  need, so enforcing it would refuse the size's own addons. Plus
  `+otel-collector`. (An `audit-logging` addon was listed here once; its chart
  never existed, and it was removed.)

#### `withAddons`

<details>
<summary>`withAddons` — function</summary>

```go
func withAddons(base []core.AddonRef, extra ...core.AddonRef) []core.AddonRef
```

- **Behavior:** returns a copy of `base` with `extra` appended, backed by a freshly allocated array — appending to `baseAddons` directly would risk one size's growth silently overwriting another's slice if their capacities ever happened to overlap.

</details>

#### `replaceAddon`

<details>
<summary>`replaceAddon` — function</summary>

```go
func replaceAddon(addons []core.AddonRef, name string, replacement core.AddonRef) []core.AddonRef
```

- **Behavior:** returns a copy of `addons` with the entry named `name` swapped for `replacement`, again without aliasing the input slice's backing array. Used by `sizeLarge` to supersede `sizeMedium`'s baseline `kyverno-policies` addon.

</details>
