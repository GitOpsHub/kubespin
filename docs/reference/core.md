# internal/core

`internal/core` holds the domain types shared by every other package in kubespin: `ClusterID`, `ClusterSpec`, `Profile`, `AddonRef`, `Access`, `NodePool`, and the provisioning phase state machine. It is deliberately dependency-free — no cloud SDKs, no I/O, no imports of other internal packages — making it a leaf every other package can import without risking an import cycle. These types flow through `internal/catalog`, `internal/orchestrator`, `internal/registry`, and the cluster repo's `cluster.yaml`/`addons.yaml` contract described in the root [CLAUDE.md](https://github.com/GitOpsHub/kubespin/blob/main/CLAUDE.md).

## Quick reference

| Name | Kind | File | Summary |
|---|---|---|---|
| [ErrInvalidSpec](#errinvalidspec) | var (sentinel) | cluster.go | Sentinel wrapping every `Validate` failure in this package. |
| [Provider](#provider) | const-block | cluster.go | The cloud a cluster is provisioned on (aws/gcp/azure). |
| [Providers](#providers) | func | cluster.go | Returns all three `Provider` values, in help-text order. |
| [Access](#access) | const-block | cluster.go | API server exposure model: private or public. |
| [ClusterID](#clusterid) | struct (string) | cluster.go | Unique fleet-wide cluster identifier. |
| [NodePool](#nodepool) | struct | cluster.go | A homogeneous group of worker nodes. |
| [ClusterSpec](#clusterspec) | struct | cluster.go | Desired state of one cluster (`cluster.yaml` contents). |
| [ErrInvalidTransition](#errinvalidtransition) | var (sentinel) | phase.go | Sentinel returned when a phase transition is illegal. |
| [Phase](#phase) | const-block | phase.go | A cluster's position in the provisioning state machine. |
| [PhaseOrder](#phaseorder) | var | phase.go | Every phase, in state machine order. |
| [CanTransition](#cantransition) | func | phase.go | Reports whether `from -> to` is a legal phase transition. |
| [ValidateTransition](#validatetransition) | func | phase.go | `CanTransition`, wrapped with a descriptive error. |
| [ProfileRef](#profileref) | struct | profile.go | Pointer to a versioned profile in the platform-profiles repo. |
| [AddonRef](#addonref) | struct | profile.go | One Helm chart delivered to a cluster. |
| [AddonOverride](#addonoverride) | struct | profile.go | Per-cluster patch onto one addon of a resolved profile. |
| [Profile](#profile) | struct | profile.go | A resolved tier from the platform-profiles catalog. |

## cluster.go

#### ErrInvalidSpec

??? warning "Signature: `ErrInvalidSpec`"

    ```go
    var ErrInvalidSpec = errors.New("invalid spec")
    ```

    - **Behavior:** sentinel wrapping every validation failure produced by this package's `Validate` methods.
    - **Invariants:** callers must branch with `errors.Is(err, core.ErrInvalidSpec)` rather than matching on message text.

#### Provider

??? abstract "Signature: `Provider`"

    ```go
    type Provider string

    const (
        ProviderAWS   Provider = "aws"
        ProviderGCP   Provider = "gcp"
        ProviderAzure Provider = "azure"
    )
    ```

    - **Behavior:** identifies the cloud a cluster is provisioned on; each has an implementation under `internal/provisioner`.

#### Providers

??? note "Signature: `Providers`, `(Provider) Valid`, `(Provider) String`"

    ```go
    func Providers() []Provider
    func (p Provider) Valid() bool
    func (p Provider) String() string
    ```

    - **Behavior:** `Providers` returns all three constants in the order help text should show them; `Valid` is true only for the three constants; `String` renders the raw value.

#### Access

??? abstract "Signature: `Access`"

    ```go
    type Access string

    const (
        AccessPrivate Access = "private"
        AccessPublic  Access = "public"
    )
    ```

    - **Behavior:** the cluster's API server exposure model.
    - **Invariants:** it is a first-class field rather than a per-cloud option because it branches behavior in two places: cluster creation (endpoint/authorized-network config, per cloud) and addon templating (internal load balancer unless `AccessPublic` combines with an external ingress exposure).

??? note "Signature: `(Access) Valid`, `(Access) String`"

    ```go
    func (a Access) Valid() bool
    func (a Access) String() string
    ```

    - **Behavior:** `Valid` is true for `AccessPrivate` or `AccessPublic`; `String` renders the raw value.

#### ClusterID

??? abstract "Signature: `ClusterID`"

    ```go
    type ClusterID string
    ```

    - **Behavior:** uniquely identifies a cluster across the whole fleet.
    - **Invariants:** it is the Fleet Registry partition key and the suffix of the cluster's repository name, so it is immutable once a cluster reaches `PhaseClusterCreated`.

??? note "Signature: `(ClusterID) Validate`, `(ClusterID) String`"

    ```go
    func (id ClusterID) Validate() error
    func (id ClusterID) String() string
    ```

    - **Behavior:** `Validate` requires a match against `^[a-z][a-z0-9-]{1,38}[a-z0-9]$` (3-40 chars, lowercase alphanumeric or hyphen, starting with a letter, ending alphanumeric — legal simultaneously as a GitHub repo suffix, a DNS label, and a cloud resource name); empty IDs get a dedicated message.

#### NodePool

??? abstract "Signature: `NodePool`"

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

    - **Behavior:** a homogeneous group of worker nodes; sizing changes here are infra diffs that resolve to a cloud SDK reconcile call, never a git commit.

??? note "Signature: `(NodePool) Validate`"

    ```go
    func (np NodePool) Validate() error
    ```

    - **Behavior:** joins every violation found rather than stopping at the first.
    - **Invariants:** `Name` required; `InstanceType` required; `MinSize >= 0`; `MaxSize >= 1`; `MinSize <= MaxSize`; `DesiredSize` within `[MinSize, MaxSize]`; `DiskSizeGB >= 0`. Cross-pool checks (unique names within a spec) live on `ClusterSpec.Validate`, not here.

#### ClusterSpec

??? abstract "Signature: `ClusterSpec`"

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

    - **Behavior:** the desired state of one cluster — the contents of `cluster.yaml` in that cluster's repository.
    - **Fields:**
        - `AuthorizedCIDRs` — restricts API server access; meaningful only for `AccessPublic` (a private cluster has no public endpoint to restrict).
        - `Subnets` — places the cluster on an existing network (subnet IDs on AWS, a subnetwork on GCP, a subnet resource ID on Azure); when empty, `EnsureNetwork` creates a network deterministically named from the cluster ID on every provider, so a resumed or repeated `apply` adopts existing resources instead of duplicating them — leaving this empty in the persisted `cluster.yaml` durably means "kubespin manages this cluster's network."
        - `VPCCIDR` — sizes the VPC kubespin creates on AWS when `Subnets` is empty; AWS-only, ignored otherwise; empty means kubespin's default.
        - `VNetCIDR` — sizes the VNet kubespin creates on Azure when `Subnets` is empty; Azure-only, ignored otherwise.
        - `SubnetCIDR` — sizes the single subnet/subnetwork kubespin creates when `Subnets` is empty, on Azure or GCP; ignored on AWS (which derives two subnets from `VPCCIDR` instead).
        - `Overrides` — per-cluster patch onto `Profile`'s resolved addon set; lives in the user-authored `cluster.yaml` rather than a separate file, since the derived `addons.yaml` is not user-edited.

??? note "Signature: `(ClusterSpec) Validate`"

    ```go
    func (s ClusterSpec) Validate() error
    ```

    - **Behavior:** joins every problem found rather than stopping at the first, so a user fixing a spec sees the full list in one run.
    - **Invariants:** `ID.Validate()`; `Provider.Valid()`; `Region` non-empty; `Access.Valid()`; `KubernetesVersion`, if set, matches `^\d+\.\d+$`; `AuthorizedCIDRs` must be empty when `Access == AccessPrivate`; at least one `NodePool`; `VPCCIDR`/`VNetCIDR`/`SubnetCIDR`, if set, must each parse as a valid CIDR; each `NodePool.Validate()` plus rejection of duplicate node pool names; `Profile.Validate()`; each `Overrides[i].Validate()` plus rejection of duplicate override addon names. Subnets themselves are not required to be non-empty — every provider is allowed to omit them, since `EnsureNetwork` creates a network when none is supplied.

## Phase state machine (phase.go)

A cluster's position in the provisioning state machine. The orchestrator resumes a failed `apply` by re-entering at the recorded phase, which is what makes retry and first run the same code path. See the [architecture doc's phase diagram](../architecture.md#the-phase-state-machine) for the visual state machine; it is not duplicated here.

#### ErrInvalidTransition

??? warning "Signature: `ErrInvalidTransition`"

    ```go
    var ErrInvalidTransition = errors.New("invalid phase transition")
    ```

    - **Behavior:** returned by `ValidateTransition` when a phase change is not legal.
    - **Invariants:** the Fleet Registry checks this on every write, so an illegal state machine move fails at the storage boundary instead of being silently persisted.

#### Phase

??? abstract "Signature: `Phase`"

    ```go
    type Phase string

    const (
        PhasePending          Phase = "pending"
        PhaseClusterCreated   Phase = "cluster-created"
        PhaseIdentityBound    Phase = "identity-bound"
        PhaseRepoPushed       Phase = "repo-pushed"
        PhaseArgoCDInstalled  Phase = "argocd-installed"
        PhaseReady            Phase = "ready"
        PhaseDecommissioning  Phase = "decommissioning"
        PhaseDecommissioned   Phase = "decommissioned"
    )
    ```

    - **Behavior:** `PhasePending` is the initial state; `PhaseReady` is the terminal happy-path state; `PhaseDecommissioning` is reachable from any live phase so a half-built cluster can still be deleted; `PhaseDecommissioned` is the only `Terminal()` phase.

??? note "Signature: `(Phase) Valid`, `(Phase) String`, `(Phase) Terminal`, `(Phase) Next`"

    ```go
    func (p Phase) Valid() bool
    func (p Phase) String() string
    func (p Phase) Terminal() bool
    func (p Phase) Next() (Phase, bool)
    ```

    - **Behavior:** `Valid` is derived from `PhaseOrder`, not the transition table, since terminal phases have no successor but are still valid phases to be in; `Terminal` is true only for `PhaseDecommissioned`; `Next` returns the happy-path successor phase from `forwardTransitions`, and `false` if `p` has none (i.e. `PhaseReady` or `PhaseDecommissioned`).

#### PhaseOrder

??? abstract "Signature: `PhaseOrder`"

    ```go
    var PhaseOrder = []Phase{
        PhasePending, PhaseClusterCreated, PhaseIdentityBound,
        PhaseRepoPushed, PhaseArgoCDInstalled, PhaseReady,
        PhaseDecommissioning, PhaseDecommissioned,
    }
    ```

    - **Behavior:** every phase, in state machine order, for display and iteration.
    - **Invariants:** this is the authoritative list of phases — `Phase.Valid` is derived from it, so a new phase constant is only recognized once it is registered here.

#### CanTransition

??? note "Signature: `CanTransition`"

    ```go
    func CanTransition(from, to Phase) bool
    ```

    - **Behavior:** reports whether `from -> to` is legal; both phases must be `Valid`.
    - **Invariants:** three rules in precedence order: (1) a phase may always transition to itself (idempotent no-op on retry); (2) any live (non-terminal) phase may enter `PhaseDecommissioning`; (3) otherwise only the single forward step recorded in `forwardTransitions` is legal — no skipping ahead, no rollback.

#### ValidateTransition

??? note "Signature: `ValidateTransition`"

    ```go
    func ValidateTransition(from, to Phase) error
    ```

    - **Behavior:** wraps `CanTransition` with a descriptive, `ErrInvalidTransition`-wrapped error — reports unknown phases by name, or `from -> to` when both are known but the move isn't allowed; returns `nil` when the transition is legal.

## profile.go

#### ProfileRef

??? abstract "Signature: `ProfileRef`"

    ```go
    type ProfileRef struct {
        Name    string
        Version string
    }
    ```

    - **Behavior:** points at a versioned profile in the platform-profiles repository.
    - **Invariants:** pinning the version is what makes `fleet update` a deliberate, staged action rather than an implicit consequence of someone merging to the catalog.

#### Profile

??? note "Signature: `(ProfileRef) Validate`, `(ProfileRef) String`"

    ```go
    func (r ProfileRef) Validate() error
    func (r ProfileRef) String() string
    ```

    - **Behavior:** `Validate` requires `Name` to match the shared name pattern (`^[a-z][a-z0-9-]{1,61}[a-z0-9]$`) and `Version` to be set; `String` renders as `"name@version"`.

#### AddonRef

??? abstract "Signature: `AddonRef`"

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

    - **Behavior:** one Helm chart delivered to a cluster; each addon becomes its own Argo CD Application, so addons sync and fail independently.
    - **Fields:** `Providers` restricts the addon to the named clouds (e.g. Karpenter, EKS-only); empty means every provider — only `Profile.ForProvider` acts on it, so a profile resolved without going through `ForProvider` still carries every addon regardless of this field.

??? note "Signature: `(AddonRef) Validate`, `(AddonRef) SupportsProvider`"

    ```go
    func (a AddonRef) Validate() error
    func (a AddonRef) SupportsProvider(provider Provider) bool
    ```

    - **Behavior:** `Validate` requires `Name` (valid name pattern), `Chart`, `Repository`, `Namespace`, and each entry in `Providers` to be `Valid()`; `SupportsProvider` is true when `Providers` is empty or contains `provider`.
    - **Invariants:** `Version` is mandatory — an unpinned addon would make a cluster's resolved state unreproducible, breaking the `.state.yaml` no-op guarantee.

#### AddonOverride

??? abstract "Signature: `AddonOverride`"

    ```go
    type AddonOverride struct {
        Name    string
        Version string         // optional
        Values  map[string]any // optional
        Disable bool           // optional
    }
    ```

    - **Behavior:** patches one addon of a resolved profile, by name, as part of a cluster's per-cluster override patch; `Disable` drops the addon from the resolved set entirely.
    - **Invariants:** every field but `Name` is optional and additive (a zero `Version` leaves the profile's pinned version alone, a nil `Values` leaves the profile's values alone); it never introduces a new addon — `Name` must match one the profile already carries, checked by `internal/catalog.Merge`, not here.

??? note "Signature: `(AddonOverride) Validate`"

    ```go
    func (o AddonOverride) Validate() error
    ```

    - **Behavior:** checks only that `Name` matches the shared name pattern.
    - **Invariants:** it cannot check that `Name` matches an addon in the profile being overridden, since that is a property of a `(profile, override)` pair, not of the override alone.

??? abstract "Signature: `Profile`"

    ```go
    type Profile struct {
        Name    string
        Version string
        Addons  []AddonRef
    }
    ```

    - **Behavior:** a resolved tier from the platform-profiles catalog — the addon set a cluster gets before any per-cluster override patch is applied.

??? note "Signature: `(Profile) Ref`, `(Profile) ForProvider`, `(Profile) Validate`"

    ```go
    func (p Profile) Ref() ProfileRef
    func (p Profile) ForProvider(provider Provider) Profile
    func (p Profile) Validate() error
    ```

    - **Behavior:** `Ref` returns `ProfileRef{Name, Version}`; `ForProvider` returns a copy of `p` with every addon that does not support `provider` dropped (via `AddonRef.SupportsProvider`); `Validate` validates `Ref()`, requires at least one addon, validates each `AddonRef`, and rejects duplicate addon names (two Argo CD Applications cannot share a name).
    - **Invariants:** callers resolve a profile for a specific cluster's provider through `ForProvider` before applying override patches, so e.g. Karpenter never renders into a GCP or Azure cluster's `addons.yaml`, and an override naming it on those clouds correctly fails as unknown rather than silently applying.

!!! note
    `namePattern` (`^[a-z][a-z0-9-]{1,61}[a-z0-9]$`) is shared by `ProfileRef`, `AddonRef`, and `AddonOverride` name validation, since all three surface as Argo CD Application names.
