# internal/provisioner/gcp

`internal/provisioner/gcp` is the GKE implementation of the three shared
provisioner interfaces defined in [`internal/provisioner/provisioner.go`](../architecture.md):
`ClusterProvisioner`, `IdentityProvisioner`, `NetworkProvisioner`, plus
`RESTConfigProvisioner` for the Argo CD installer. Every GCP service the
package touches — GKE Cluster Manager, IAM service accounts, Compute networks/
subnetworks/routers/firewalls — is reached through a narrow interface declared
in [`gcp.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/provisioner/gcp/gcp.go), so the whole provisioner is testable
without credentials and the interfaces double as the exact permission set an
operator must grant.

!!! warning "GKE quirks"
    - **`EnablePrivateNodes` is always `true`**, regardless of `Access` mode
      (only `EnablePrivateEndpoint` toggles with `AccessPrivate`) — every GKE
      cluster's nodes have no public IP and rely entirely on Cloud NAT for
      internet egress, including pulling an addon's image from a public
      registry.
    - Because of that, a kubespin-managed network (`EnsureNetwork` when
      `spec.Subnets` is empty) always provisions a Cloud Router + Cloud NAT
      alongside the VPC and subnetwork ([`network.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/provisioner/gcp/network.go)).
    - **GKE's master-authorized-networks allowlist is empty by default**:
      setting `Access: public` alone does not make the endpoint reachable by
      anyone, including the operator running `apply` — CIDRs must be supplied
      via `--authorized-cidrs` for `authorizedNetworksConfig` to enable the
      allowlist at all ([`cluster.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/provisioner/gcp/cluster.go)).

## Quick reference

| Name | Kind | File | Summary |
| --- | --- | --- | --- |
| [`Clients`](#clients) | type | gcp.go | Bundles the project-scoped GCP clients the provisioner uses. |
| [`Option`](#option) | type | gcp.go | Functional option for `NewClients`. |
| [`NewClients`](#newclients) | func | gcp.go | Builds real GCP clients for a project. |
| [`ClusterProvisioner`](#clusterprovisioner) | type | cluster.go | Creates/reconciles GKE clusters and node pools. |
| [`NewClusterProvisioner`](#newclusterprovisioner) | func | cluster.go | Wraps `Clients` as a `ClusterProvisioner`. |
| [`(*ClusterProvisioner) Provider`](#clusterprovisioner-provider) | method | cluster.go | Returns `core.ProviderGCP`. |
| [`(*ClusterProvisioner) Create`](#clusterprovisioner-create) | method | cluster.go | Validates spec, creates or reconciles node pools. |
| [`(*ClusterProvisioner) Describe`](#clusterprovisioner-describe) | method | cluster.go | Reads current GKE cluster state. |
| [`(*ClusterProvisioner) Reconcile`](#clusterprovisioner-reconcile) | method | cluster.go | Reconciles access mode and node pools. |
| [`(*ClusterProvisioner) Delete`](#clusterprovisioner-delete) | method | cluster.go | Idempotently deletes the cluster. |
| [`(*ClusterProvisioner) RESTConfig`](#clusterprovisioner-restconfig) | method | kubeauth.go | Builds a `*rest.Config` for the Argo CD installer. |
| [`IdentityProvisioner`](#identityprovisioner) | type | identity.go | Binds in-cluster service accounts via Workload Identity. |
| [`NewIdentityProvisioner`](#newidentityprovisioner) | func | identity.go | Wraps `Clients` as an `IdentityProvisioner`. |
| [`(*IdentityProvisioner) Provider`](#identityprovisioner-provider) | method | identity.go | Returns `core.ProviderGCP`. |
| [`(*IdentityProvisioner) ProvisionForComponent`](#identityprovisioner-provisionforcomponent) | method | identity.go | Ensures a GSA and binds Workload Identity. |
| [`(*IdentityProvisioner) Deprovision`](#identityprovisioner-deprovision) | method | identity.go | Deletes a component's GSA. |
| [`NetworkProvisioner`](#networkprovisioner) | type | network.go | Resolves/creates the VPC network and subnetwork. |
| [`NewNetworkProvisioner`](#newnetworkprovisioner) | func | network.go | Wraps `Clients` as a `NetworkProvisioner`. |
| [`(*NetworkProvisioner) Provider`](#networkprovisioner-provider) | method | network.go | Returns `core.ProviderGCP`. |
| [`(*NetworkProvisioner) EnsureNetwork`](#networkprovisioner-ensurenetwork) | method | network.go | Creates or adopts VPC/subnetwork/Cloud NAT. |
| [`(*NetworkProvisioner) AllowEgress`](#networkprovisioner-allowegress) | method | network.go | Opens the status-reporter's outbound firewall rule. |

## gcp.go

#### `Clients`

??? abstract "type Clients struct { ... }"
    ```go
    type Clients struct {
        project     string
        cluster     clusterAPI
        svcAccts    serviceAccountsAPI
        firewalls   firewallsAPI
        networks    networksAPI
        subnetworks subnetworksAPI
        routers     routersAPI
        tokens      tokenAPI

        logger *slog.Logger
    }
    ```

    - Bundles the GCP clients the provisioner uses, scoped to one project.
    - The project is fixed at construction — like AWS's `Clients` fixes a
      region — because a cluster's spec carries its location (region) but not
      the project that owns it, which is operator configuration rather than
      cluster desired state.
    - All fields are unexported; construct one with `NewClients`.
    - `logger` defaults to `slog.Default()` and every provisioner built over a
      `Clients` logs through it.

#### `Option`

??? abstract "type Option func(*Clients)"
    ```go
    type Option func(*Clients)
    ```

    - Functional option for `NewClients`.
    - `WithLogger(logger *slog.Logger) Option` is the only option defined.

#### `NewClients`

??? note "func NewClients(ctx context.Context, project string, opts ...Option) (*Clients, error)"
    ```go
    func NewClients(ctx context.Context, project string, opts ...Option) (*Clients, error)
    ```

    - Builds real GCP clients (GKE Cluster Manager, IAM, Compute) for `project`.
    - Returns an error if `project` is empty or any underlying client fails to
      build.
    - Applies `opts` after defaults are set.

??? abstract "type names struct { ... } (internal)"
    ```go
    type names struct {
        project string
        spec    core.ClusterSpec
    }
    ```

    - Derives every GCP resource name from the cluster ID (`spec.ID`) so a
      cluster's resources are identifiable and a second cluster cannot
      collide with them.
    - Methods: `location`, `parent`, `cluster`, `clusterPath`, `nodePool`,
      `nodePoolPath`, `serviceAccountID`, `serviceAccountEmail`,
      `serviceAccountResource`, `network` (`kubespin-<clusterID>`),
      `networkResource`, `subnetwork` (`kubespin-<clusterID>-subnet`),
      `subnetworkResource`, `router` (`kubespin-<clusterID>-router`), `nat`
      (`kubespin-<clusterID>-nat`).
    - `serviceAccountID` truncates `"ksp-" + clusterID + "-" + component` to
      30 characters to satisfy IAM's 6–30 char service-account-ID limit.

??? note "Internal client-facade interfaces"
    Not part of the exported API, but they are the exact permission surface
    this package needs and are worth citing for anyone auditing IAM grants:

    - `clusterAPI` — GKE Cluster Manager: `GetCluster`, `CreateCluster`,
      `UpdateCluster`, `DeleteCluster`, `ListNodePools`, `GetNodePool`,
      `CreateNodePool`, `SetNodePoolSize`, `DeleteNodePool`.
    - `serviceAccountsAPI` — IAM service accounts: `Get`, `Create`, `Delete`,
      `GetIamPolicy`, `SetIamPolicy`.
    - `firewallsAPI` — used only for the status reporter's egress rule:
      `GetFirewall`, `Insert`. Named `GetFirewall` rather than `Get` because a
      single fake stands in for both this and `serviceAccountsAPI` in tests,
      and Go disallows two same-named methods with different signatures on
      one type.
    - `networksAPI` / `subnetworksAPI` / `routersAPI` — used only by
      `EnsureNetwork` when `spec.Subnets` is empty: `GetNetwork`/
      `InsertNetwork`, `GetSubnetwork`/`InsertSubnetwork`, `GetRouter`/
      `InsertRouter`.
    - `tokenAPI` (in `kubeauth.go`) — `Token(ctx) (string, error)`, narrowed
      to one operation so `RESTConfig` is testable without real Google
      credentials.

    Each has a `real*` adapter (`realServiceAccounts`, `realFirewalls`,
    `realNetworks`, `realSubnetworks`, `realRouters`) wrapping the fluent
    `google.golang.org/api/*` clients, and `applicationDefaultTokens` wraps
    `golang.org/x/oauth2/google.FindDefaultCredentials`.

??? note "Package-level helpers"
    - `sanitizeLabelValue(s string) string` / `labels(spec) map[string]string`
      — build GKE resource labels (`managed-by`, `kubespin-cluster`,
      `kubespin-size`), lowercasing and replacing disallowed characters so
      a size string like `"small"` fits GCP's label-value character
      rules.

## cluster.go

#### `ClusterProvisioner`

??? abstract "type ClusterProvisioner struct { ... }"
    ```go
    type ClusterProvisioner struct {
        c *Clients
    }
    ```

    - Implements `provisioner.ClusterProvisioner` and
      `provisioner.RESTConfigProvisioner` (the latter in `kubeauth.go`).
    - Creates and reconciles GKE clusters and their node pools.

    Invariants:

    - `Create`/`Delete` are idempotent: an existing cluster, an existing node
      pool, or an already-deleting cluster is treated as convergence, not
      error (`codes.AlreadyExists`, `codes.FailedPrecondition` on a raced
      delete).
    - `Create` always requests a placeholder node pool from
      `spec.NodePools[0]` (or a bare `default` pool if none are configured)
      because `CreateCluster` requires at least one; real pools are
      reconciled by `ensureNodePools` once the control plane is active.
    - `Reconcile` never deletes a node pool — only creates missing ones and
      resizes drifted ones — because deleting one evicts running workloads, a
      decision left to a human.
    - `Reconcile`/`Create` report `provisioner.Change` as data (never
      inferred by diffing before/after state), so a no-op `apply` can prove
      it made zero cloud calls.

#### `NewClusterProvisioner`

??? note "func NewClusterProvisioner(c *Clients) *ClusterProvisioner"
    ```go
    func NewClusterProvisioner(c *Clients) *ClusterProvisioner
    ```

    - Wraps `c` as a `ClusterProvisioner`.

#### `(*ClusterProvisioner) Provider`

??? note "func (p *ClusterProvisioner) Provider() core.Provider"
    ```go
    func (p *ClusterProvisioner) Provider() core.Provider
    ```

    - Returns `core.ProviderGCP`.

#### `(*ClusterProvisioner) Create`

??? note "func (p *ClusterProvisioner) Create(ctx context.Context, spec core.ClusterSpec) error"
    ```go
    func (p *ClusterProvisioner) Create(ctx context.Context, spec core.ClusterSpec) error
    ```

    - Validates the spec (`validateForGKE`, requiring `spec.Subnets` to be
      non-empty — GKE requires a subnetwork).
    - Describes current state, and either creates the cluster
      (`StatusAbsent`) or reconciles node pools (`StatusActive`).
    - No-ops while the cluster is still creating/updating/deleting.

#### `(*ClusterProvisioner) Describe`

??? note "func (p *ClusterProvisioner) Describe(ctx context.Context, spec core.ClusterSpec) (provisioner.ClusterState, error)"
    ```go
    func (p *ClusterProvisioner) Describe(ctx context.Context, spec core.ClusterSpec) (provisioner.ClusterState, error)
    ```

    - Calls `GetCluster`; a `codes.NotFound` gRPC status maps to
      `provisioner.StatusAbsent` (not an error).
    - Populates `Endpoint`, `Version`, `Access` (via `accessFrom`),
      `NetworkID`, a synthesized `OIDCIssuer`
      (`https://container.googleapis.com/v1/<clusterPath>`, GKE's fixed
      workload-pool issuer URL rather than something `CreateCluster`
      returns), and `CertificateAuthorityData` (base64-decoded from
      `MasterAuth.ClusterCaCertificate`).
    - Node pools are listed only when the cluster is `StatusActive`.

#### `(*ClusterProvisioner) Reconcile`

??? note "func (p *ClusterProvisioner) Reconcile(ctx context.Context, spec core.ClusterSpec) (provisioner.Change, error)"
    ```go
    func (p *ClusterProvisioner) Reconcile(ctx context.Context, spec core.ClusterSpec) (provisioner.Change, error)
    ```

    - Describes the cluster (errors with `provisioner.ErrNotFound` if
      absent).
    - Reconciles access mode (`reconcileAccess`) and node pools
      (`ensureNodePools`), merging both `Change`s.

#### `(*ClusterProvisioner) Delete`

??? note "func (p *ClusterProvisioner) Delete(ctx context.Context, spec core.ClusterSpec) error"
    ```go
    func (p *ClusterProvisioner) Delete(ctx context.Context, spec core.ClusterSpec) error
    ```

    - No-ops if the cluster is already absent or deleting (`alreadyGoing`).
    - Otherwise calls `DeleteCluster`; a `codes.NotFound` is treated as
      success, and a `codes.FailedPrecondition` (lost a race with another
      teardown) rechecks via `alreadyGoing` before deciding whether it's
      actually an error.
    - Deletion is asynchronous — callers poll `Describe` via
      `provisioner.WaitUntilGone`.
    - GKE deletes node pools along with the cluster, so unlike EKS there is
      no separate node-pool teardown step.

??? note "Package-level helpers"
    - `privateClusterConfig(spec) *containerpb.PrivateClusterConfig` —
      always sets `EnablePrivateNodes: true`; sets `EnablePrivateEndpoint`
      only when `spec.Access == core.AccessPrivate`.
    - `authorizedNetworksConfig(spec) *containerpb.MasterAuthorizedNetworksConfig` —
      returns `nil` unless `spec.Access == core.AccessPublic` and
      `spec.AuthorizedCIDRs` is non-empty; otherwise GKE's
      master-authorized-networks allowlist stays enabled-empty by default,
      blocking every caller including the operator.
    - `accessFrom(cfg *containerpb.PrivateClusterConfig) core.Access` —
      reads the deprecated but still-honored `EnablePrivateEndpoint` field
      (migration to `ControlPlaneEndpointsConfig` tracked separately, not
      folded into M2).
    - `subnetworkNetwork(ctx, spec, subnet) (string, error)` — looks up
      which VPC a given subnetwork belongs to, because GKE's `CreateCluster`
      does not infer `Network` from `Subnetwork` alone (an unset `Network`
      defaults to the project's `default` VPC, rejecting a non-default
      subnetwork).

## identity.go

#### `IdentityProvisioner`

??? abstract "type IdentityProvisioner struct { ... }"
    ```go
    type IdentityProvisioner struct {
        c *Clients
    }
    ```

    - Implements `provisioner.IdentityProvisioner`.
    - Binds in-cluster Kubernetes service accounts to Google service accounts
      via Workload Identity.

    Invariants:

    - The bound Google service account carries no IAM permission policy — it
      exists only so a component (`fleet-status-reporter`) can prove which
      cluster it is when signing its push, not to grant it GCP access.
    - Workload Identity needs no separate OIDC provider registration the way
      AWS IRSA does: GKE's workload pool (`<project>.svc.id.goog`) is the
      trust root for every cluster in the project, so the entire binding is
      one IAM policy member (`roles/iam.workloadIdentityUser`) scoped to
      `serviceAccount:<project>.svc.id.goog[<namespace>/<serviceaccount>]`.
    - `ProvisionForComponent` requires the cluster to already be
      `StatusActive` (returns `provisioner.ErrNotFound` wrapped otherwise) —
      binding identity before the cluster is usable is not meaningful work,
      kept as a separate phase like AWS's IRSA binding.
    - The IAM policy is rewritten in place (existing binding's `Members`
      appended to, or a new `Binding` appended to `policy.Bindings`) rather
      than replaced wholesale, so unrelated bindings an operator added by
      hand survive `apply`.
    - `Deprovision` deletes the service account, which also removes its IAM
      policy bindings; deleting an absent one is a no-op (HTTP 404).

#### `NewIdentityProvisioner`

??? note "func NewIdentityProvisioner(c *Clients) *IdentityProvisioner"
    ```go
    func NewIdentityProvisioner(c *Clients) *IdentityProvisioner
    ```

    - Wraps `c` as an `IdentityProvisioner`.

#### `(*IdentityProvisioner) Provider`

??? note "func (p *IdentityProvisioner) Provider() core.Provider"
    ```go
    func (p *IdentityProvisioner) Provider() core.Provider
    ```

    - Returns `core.ProviderGCP`.

#### `(*IdentityProvisioner) ProvisionForComponent`

??? note "func (p *IdentityProvisioner) ProvisionForComponent(...) (provisioner.Binding, error)"
    ```go
    func (p *IdentityProvisioner) ProvisionForComponent(
        ctx context.Context, spec core.ClusterSpec, comp provisioner.Component,
    ) (provisioner.Binding, error)
    ```

    - Requires the cluster `StatusActive`.
    - Ensures the Google service account exists (`ensureServiceAccount`),
      binds Workload Identity (`bindWorkloadIdentity`).
    - Returns a `provisioner.Binding` whose `Identifier` is the service
      account email and whose `Annotations` carries
      `iam.gke.io/gcp-service-account: <email>` — the key the caller applies
      blind to the Kubernetes ServiceAccount to complete the binding.

#### `(*IdentityProvisioner) Deprovision`

??? note "func (p *IdentityProvisioner) Deprovision(...) error"
    ```go
    func (p *IdentityProvisioner) Deprovision(
        ctx context.Context, spec core.ClusterSpec, comp provisioner.Component,
    ) error
    ```

    - Deletes the component's Google service account (which drops its IAM
      policy bindings too).
    - A 404 is treated as success.

??? note "Package-level helpers"
    - `code(err error) int` — extracts the HTTP status from a
      `googleapi.Error`, since REST-based GCP clients (IAM, Compute) report
      errors this way, unlike the gRPC-based GKE client (which uses
      `google.golang.org/grpc/status` codes instead).

## network.go

#### `NetworkProvisioner`

??? abstract "type NetworkProvisioner struct { ... }"
    ```go
    type NetworkProvisioner struct {
        c       *Clients
        cluster *ClusterProvisioner
    }
    ```

    - Implements `provisioner.NetworkProvisioner`.
    - Resolves/creates the VPC network and subnetwork a cluster is created
      in, and opens the one outbound firewall rule fleet state depends on.

    Invariants:

    - `EnsureNetwork` passes `spec.Subnets` through unchanged when already
      set — the operator owns that network, kubespin never touches it.
    - When `spec.Subnets` is empty, it creates (idempotently, adopting on
      repeat) a custom-mode VPC network, one regional subnetwork, and a
      Cloud Router + Cloud NAT — all deterministically named from the
      cluster ID via `names`, so a resumed or repeated `apply` converges
      instead of duplicating resources.
    - `AllowEgress` requires the cluster to already have a network
      (`state.NetworkID` populated), i.e. cluster creation (or at least
      `EnsureNetwork`) must precede it; an existing firewall rule with the
      cluster's deterministic name is left alone, so a resumed apply does
      not accumulate duplicate rules.
    - Compute Engine v1 `Insert` calls are long-running operations: a
      successful `Do()` only means the request was accepted.
      `waitGlobalOperation` / `waitRegionOperation` poll until
      `Status == "DONE"` and surface any embedded `op.Error`, because a
      mid-operation conflict (e.g. a CIDR already in use) is reported that
      way, not as an HTTP error.
    - Subnetwork creation retries on HTTP 400 `resourceNotReady`
      (`isResourceNotReady`) up to `subnetworkReadyRetries` (6) times with
      `subnetworkReadyBackoff` (3s) between attempts, because a freshly
      created VPC network can take a few seconds to propagate before it
      accepts a subnetwork insert.

#### `NewNetworkProvisioner`

??? note "func NewNetworkProvisioner(c *Clients) *NetworkProvisioner"
    ```go
    func NewNetworkProvisioner(c *Clients) *NetworkProvisioner
    ```

    - Wraps `c` as a `NetworkProvisioner`, also constructing an internal
      `ClusterProvisioner` (used by `AllowEgress` to read `state.NetworkID`).

#### `(*NetworkProvisioner) Provider`

??? note "func (p *NetworkProvisioner) Provider() core.Provider"
    ```go
    func (p *NetworkProvisioner) Provider() core.Provider
    ```

    - Returns `core.ProviderGCP`.

#### `(*NetworkProvisioner) EnsureNetwork`

??? note "func (p *NetworkProvisioner) EnsureNetwork(...) (provisioner.NetworkResult, error)"
    ```go
    func (p *NetworkProvisioner) EnsureNetwork(
        ctx context.Context, spec core.ClusterSpec,
    ) (provisioner.NetworkResult, error)
    ```

    - Returns `spec.Subnets` unchanged if already set.
    - Otherwise ensures (create-or-adopt) a custom-mode VPC network
      (`ensureVPCNetwork`), a regional subnetwork with
      `PrivateIpGoogleAccess: true` at `spec.SubnetCIDR` or
      `defaultSubnetCIDR` (`10.0.0.0/20`) (`ensureSubnetwork`), and a Cloud
      Router + Cloud NAT (`ensureCloudNAT`) — required because GKE nodes
      always run with `EnablePrivateNodes`.
    - Returns the subnetwork's resource path as the single `SubnetIDs`
      entry.

#### `(*NetworkProvisioner) AllowEgress`

??? note "func (p *NetworkProvisioner) AllowEgress(...) (provisioner.Change, error)"
    ```go
    func (p *NetworkProvisioner) AllowEgress(
        ctx context.Context, spec core.ClusterSpec, dest provisioner.EgressDestination,
    ) (provisioner.Change, error)
    ```

    - Describes the cluster to obtain `state.NetworkID` (errors with
      `provisioner.ErrNotFound` if the cluster has no network yet).
    - Idempotently creates an `EGRESS` VPC firewall rule named
      `kubespin-<clusterID>-egress` allowing TCP to `dest.CIDR` (default
      `0.0.0.0/0`) on `dest.Port` (default `443`), tagged with the cluster
      ID.

??? note "Package-level helpers"
    - `isResourceNotReady(err error) bool` — detects the transient 400
      `resourceNotReady` reason GCP returns while a just-created network
      propagates.

## kubeauth.go

#### `(*ClusterProvisioner) RESTConfig`

??? note "func (p *ClusterProvisioner) RESTConfig(ctx context.Context, spec core.ClusterSpec) (*rest.Config, error)"
    ```go
    func (p *ClusterProvisioner) RESTConfig(ctx context.Context, spec core.ClusterSpec) (*rest.Config, error)
    ```

    - Satisfies `provisioner.RESTConfigProvisioner`, used by the Argo CD
      installer.
    - Requires the cluster to be `StatusActive` (re-describes to get
      `Endpoint` and `CertificateAuthorityData`).
    - Mints a bearer token via `p.c.tokens.Token`, which in production wraps
      `google.FindDefaultCredentials` scoped to
      `https://www.googleapis.com/auth/cloud-platform` — the same scope
      `gcloud container clusters get-credentials` requests.
    - No credential is minted or stored by this package; it reuses whatever
      `gcloud auth application-default login` already cached.
