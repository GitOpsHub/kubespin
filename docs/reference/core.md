# internal/core

`internal/core` holds the domain types shared by every other package in
kubespin: `ClusterID`, `ClusterSpec`, `Profile`, `AddonRef`, `Access`,
`NodePool`, and the provisioning phase state machine. It is deliberately
dependency-free — no cloud SDKs, no I/O, no imports of other internal
packages — which makes it a leaf that everything else in the tree can import
without risking an import cycle. These are the types that flow through
`internal/catalog` (profile resolution), `internal/orchestrator` (the phase
machine), `internal/registry` (the Fleet Registry's stored shape), and the
cluster repo's `cluster.yaml`/`addons.yaml` contract described in the root
[CLAUDE.md](https://github.com/GitOpsHub/kubespin/blob/main/CLAUDE.md).

## Errors

### `ErrInvalidSpec`

Sentinel wrapping every validation failure produced by this package's
`Validate` methods. Callers branch with `errors.Is(err, core.ErrInvalidSpec)`
rather than matching on message text. (cluster.go)

### `ErrInvalidTransition`

Sentinel returned by `ValidateTransition` when a phase change is not legal.
The Fleet Registry checks this on every write, so an illegal state machine
move fails at the storage boundary instead of being silently persisted.
(phase.go)

## Cluster identity and shape (cluster.go)

### `Provider`

```go
type Provider string
```

The cloud a cluster is provisioned on.

| Constant | Value |
|---|---|
| `ProviderAWS` | `"aws"` |
| `ProviderGCP` | `"gcp"` |
| `ProviderAzure` | `"azure"` |

Each has an implementation under `internal/provisioner`.

- `Providers() []Provider` — returns all three, in the order help text should show them.
- `(Provider) Valid() bool` — true only for the three constants above.
- `(Provider) String() string`

### `Access`

```go
type Access string
```

The cluster's API server exposure model: `AccessPrivate` (`"private"`) or
`AccessPublic` (`"public"`). It is a first-class field rather than a
per-cloud option because it branches behavior in two places: cluster
creation (endpoint/authorized-network config, per cloud) and addon
templating (internal load balancer unless `AccessPublic` combines with an
external ingress exposure).

- `(Access) Valid() bool` — true for `AccessPrivate` or `AccessPublic`.
- `(Access) String() string`

### `ClusterID`

```go
type ClusterID string
```

Uniquely identifies a cluster across the whole fleet. It is the Fleet
Registry partition key and the suffix of the cluster's repository name, so it
is immutable once a cluster reaches `PhaseClusterCreated`.

- `(ClusterID) Validate() error` — must match `^[a-z][a-z0-9-]{1,38}[a-z0-9]$`
  (3–40 chars, lowercase alphanumeric or hyphen, starting with a letter,
  ending alphanumeric — legal simultaneously as a GitHub repo suffix, a DNS
  label, and a cloud resource name). Empty IDs are rejected with a dedicated
  message.
- `(ClusterID) String() string`

### `NodePool`

```go
type NodePool struct {
    Name         string
    InstanceType string
    MinSize      int32
    MaxSize      int32
    DesiredSize  int32
    DiskSizeGB   int32             // optional
    Labels       map[string]string // optional
}
```

A homogeneous group of worker nodes. Sizing changes here are infra diffs:
they resolve to a cloud SDK reconcile call, never to a git commit.

`(NodePool) Validate() error` enforces, joining every violation found:

- `Name` is required.
- `InstanceType` is required.
- `MinSize >= 0`.
- `MaxSize >= 1`.
- `MinSize <= MaxSize`.
- `DesiredSize` within `[MinSize, MaxSize]`.
- `DiskSizeGB >= 0`.

Cross-pool checks (unique names within a spec) are not done here — they live
on `ClusterSpec.Validate`.

### `ClusterSpec`

```go
type ClusterSpec struct {
    ID                ClusterID
    Provider          Provider
    Region            string
    Access            Access
    KubernetesVersion string          // optional, "MAJOR.MINOR"
    NodePools         []NodePool
    Profile           ProfileRef
    AuthorizedCIDRs   []string        // optional
    Subnets           []string
    VPCCIDR           string          // optional, AWS only
    VNetCIDR          string          // optional, Azure only
    SubnetCIDR        string          // optional, Azure/GCP only
    Overrides         []AddonOverride // optional
}
```

The desired state of one cluster — the contents of `cluster.yaml` in that
cluster's repository.

| Field | Meaning |
|---|---|
| `ID` | Cluster identity; see `ClusterID`. |
| `Provider` | Target cloud. |
| `Region` | Cloud region. |
| `Access` | `private` or `public` API server exposure. |
| `KubernetesVersion` | Optional; if set, must match `MAJOR.MINOR`. |
| `NodePools` | At least one required. |
| `Profile` | Reference to the resolved addon profile (see `ProfileRef`). |
| `AuthorizedCIDRs` | Restricts API server access; meaningful only for `AccessPublic` — a private cluster has no public endpoint to restrict. |
| `Subnets` | Places the cluster on an existing network (subnet IDs on AWS, a subnetwork on GCP, a subnet resource ID on Azure). When empty, `EnsureNetwork` creates a network deterministically named from the cluster ID on every provider, so a resumed or repeated `apply` adopts existing resources instead of duplicating them. Leaving this empty in the persisted `cluster.yaml` is what durably means "kubespin manages this cluster's network." |
| `VPCCIDR` | Sizes the VPC kubespin creates on AWS when `Subnets` is empty. AWS-only; ignored otherwise. Empty means kubespin's default. |
| `VNetCIDR` | Sizes the VNet kubespin creates on Azure when `Subnets` is empty. Azure-only; ignored otherwise. |
| `SubnetCIDR` | Sizes the single subnet/subnetwork kubespin creates when `Subnets` is empty, on Azure or GCP. Ignored on AWS (which derives two subnets from `VPCCIDR` instead). |
| `Overrides` | Per-cluster patch onto `Profile`'s resolved addon set. Lives in the user-authored `cluster.yaml` rather than a separate file, since the derived `addons.yaml` is not user-edited. |

`(ClusterSpec) Validate() error` joins every problem found rather than
stopping at the first, so a user fixing a spec sees the full list in one run.
Checks performed:

- `ID.Validate()`.
- `Provider.Valid()`.
- `Region` non-empty.
- `Access.Valid()`.
- `KubernetesVersion`, if set, matches `^\d+\.\d+$`.
- `AuthorizedCIDRs` must be empty when `Access == AccessPrivate` (rejected as meaningless otherwise).
- At least one `NodePool`.
- `VPCCIDR`, `VNetCIDR`, `SubnetCIDR`, if set, must each parse as a valid CIDR.
- Each `NodePool.Validate()`, plus rejection of duplicate node pool names.
- `Profile.Validate()`.
- Each `Overrides[i].Validate()`, plus rejection of duplicate override addon names.

Subnets themselves are not required to be validated as non-empty — every
provider is allowed to omit them, since `EnsureNetwork` creates a network
when none is supplied.

## Phase state machine (phase.go)

A cluster's position in the provisioning state machine. The orchestrator
resumes a failed `apply` by re-entering at the recorded phase, which is what
makes retry and first run the same code path. See the
[architecture doc's phase diagram](../architecture.md#the-phase-state-machine)
for the visual state machine; it is not duplicated here.

### `Phase`

```go
type Phase string
```

| Constant | Value | Notes |
|---|---|---|
| `PhasePending` | `"pending"` | Initial state. |
| `PhaseClusterCreated` | `"cluster-created"` | |
| `PhaseIdentityBound` | `"identity-bound"` | |
| `PhaseRepoPushed` | `"repo-pushed"` | |
| `PhaseArgoCDInstalled` | `"argocd-installed"` | |
| `PhaseReady` | `"ready"` | Terminal happy-path state. |
| `PhaseDecommissioning` | `"decommissioning"` | Reachable from any live phase, so a half-built cluster can still be deleted. |
| `PhaseDecommissioned` | `"decommissioned"` | The only `Terminal()` phase. |

Methods:

```go
func (p Phase) Valid() bool
func (p Phase) String() string
func (p Phase) Terminal() bool
func (p Phase) Next() (Phase, bool)
```

- `Valid` — derived from `PhaseOrder`, not the transition table, since
  terminal phases have no successor but are still valid phases to be in.
- `Terminal` — true only for `PhaseDecommissioned`.
- `Next` — returns the happy-path successor phase from `forwardTransitions`,
  and `false` if `p` has none (i.e. `PhaseReady` or `PhaseDecommissioned`).

### `PhaseOrder`

```go
var PhaseOrder = []Phase{
    PhasePending, PhaseClusterCreated, PhaseIdentityBound,
    PhaseRepoPushed, PhaseArgoCDInstalled, PhaseReady,
    PhaseDecommissioning, PhaseDecommissioned,
}
```

Every phase, in state machine order, for display and iteration. This is the
authoritative list of phases: `Phase.Valid` is derived from it, so a new
phase constant is only recognized once it is registered here.

### `CanTransition`

```go
func CanTransition(from, to Phase) bool
```

Reports whether `from -> to` is legal. Both phases must be `Valid`. Three
rules, in precedence order:

1. A phase may always transition to itself (idempotent no-op on retry).
2. Any live (non-terminal) phase may enter `PhaseDecommissioning`.
3. Otherwise only the single forward step recorded in `forwardTransitions` is
   legal — no skipping ahead, no rollback.

### `ValidateTransition`

```go
func ValidateTransition(from, to Phase) error
```

Wraps `CanTransition` with a descriptive, `ErrInvalidTransition`-wrapped
error: reports unknown phases by name, or `from -> to` when both are known
but the move isn't allowed. Returns `nil` when the transition is legal.

## Profiles and addons (profile.go)

### `ProfileRef`

```go
type ProfileRef struct {
    Name    string
    Version string
}
```

Points at a versioned profile in the platform-profiles repository. Pinning
the version is what makes `fleet update` a deliberate, staged action rather
than an implicit consequence of someone merging to the catalog.

- `(ProfileRef) Validate() error` — `Name` must match the shared name pattern
  (`^[a-z][a-z0-9-]{1,61}[a-z0-9]$`); `Version` is required.
- `(ProfileRef) String() string` — renders as `"name@version"`.

### `AddonRef`

```go
type AddonRef struct {
    Name       string
    Chart      string
    Repository string
    Version    string
    Namespace  string
    Values     map[string]any
    Providers  []Provider // optional
}
```

One Helm chart delivered to a cluster. Each addon becomes its own Argo CD
Application, so addons sync and fail independently.

| Field | Meaning |
|---|---|
| `Providers` | Restricts the addon to the named clouds, e.g. Karpenter (EKS-only). Empty means every provider. Only `Profile.ForProvider` acts on it — a profile resolved without going through `ForProvider` still carries every addon regardless of this field. |

`(AddonRef) Validate() error` requires `Name` (valid name pattern), `Chart`,
`Repository`, `Version` (mandatory — an unpinned addon would make a cluster's
resolved state unreproducible, breaking the `.state.yaml` no-op guarantee),
and `Namespace`; each entry in `Providers` must be `Valid()`.

`(AddonRef) SupportsProvider(provider Provider) bool` — true when `Providers`
is empty or contains `provider`.

### `AddonOverride`

```go
type AddonOverride struct {
    Name    string
    Version string         // optional
    Values  map[string]any // optional
    Disable bool            // optional
}
```

Patches one addon of a resolved profile, by name, as part of a cluster's
per-cluster override patch. Every field but `Name` is optional and additive:
a zero `Version` leaves the profile's pinned version alone, a nil `Values`
leaves the profile's values alone. This lets an override say only what it
changes, and it never introduces a new addon — `Name` must match one the
profile already carries (checked by `internal/catalog.Merge`, not here).
`Disable` drops the addon from the resolved set entirely.

`(AddonOverride) Validate() error` checks only that `Name` matches the shared
name pattern — it cannot check that `Name` matches an addon in the profile
being overridden, since that is a property of a `(profile, override)` pair,
not of the override alone.

### `Profile`

```go
type Profile struct {
    Name    string
    Version string
    Addons  []AddonRef
}
```

A resolved tier from the platform-profiles catalog: the addon set a cluster
gets before any per-cluster override patch is applied.

- `(Profile) Ref() ProfileRef` — returns `ProfileRef{Name, Version}`.
- `(Profile) ForProvider(provider Provider) Profile` — returns a copy of `p`
  with every addon that does not support `provider` dropped (via
  `AddonRef.SupportsProvider`). Callers resolve a profile for a specific
  cluster's provider through this before applying override patches, so e.g.
  Karpenter never renders into a GCP or Azure cluster's `addons.yaml`, and an
  override naming it on those clouds correctly fails as unknown rather than
  silently applying.
- `(Profile) Validate() error` — validates `Ref()`, requires at least one
  addon, validates each `AddonRef`, and rejects duplicate addon names (two
  Argo CD Applications cannot share a name).

!!! note
    `namePattern` (`^[a-z][a-z0-9-]{1,61}[a-z0-9]$`) is shared by
    `ProfileRef`, `AddonRef`, and `AddonOverride` name validation, since all
    three surface as Argo CD Application names.
