# internal/provisioner/azure

This package implements `provisioner.ClusterProvisioner`, `provisioner.IdentityProvisioner`,
`provisioner.NetworkProvisioner`, and `provisioner.RESTConfigProvisioner` for AKS — those
shared interfaces are defined in `internal/provisioner/provisioner.go` and documented
elsewhere; this page only covers the Azure-specific implementation behind them. AKS clusters
get a system-assigned identity plus AAD Workload Identity, and `EnsureNetwork` always ensures
the cluster's resource group first, auto-creating a VNet/subnet only when the operator hasn't
supplied one.

## Quick reference

| Name | Kind | File | Summary |
|---|---|---|---|
| [`clusterAPI`](#clusterapi) | interface | azure.go | AKS surface: cluster CRUD, agent pool CRUD, kubeconfig retrieval |
| [`identityAPI`](#identityapi) | interface | azure.go | Managed identity + federated credential CRUD |
| [`networkAPI`](#networkapi) | interface | azure.go | NSG/security rule and VNet/subnet CRUD |
| [`resourceGroupAPI`](#resourcegroupapi) | interface | azure.go | Resource group existence check + ensure |
| [`Clients`](#clients) | struct | azure.go | Bundles Azure clients scoped to one subscription |
| [`Option`](#option) | func type | azure.go | Functional option for configuring `Clients` |
| [`names`](#names) | struct | azure.go | Derives every Azure resource name from the cluster ID |
| [`NewClients`](#newclients) | func | azure.go | Builds real Azure clients, authenticating via `DefaultAzureCredential` |
| [Helper functions](#helper-functions) | func | azure.go | `code`, `tags`, `ptr`/`deref`/... pointer/tag utilities |
| [`ClusterProvisioner`](#clusterprovisioner) | struct | cluster.go | Implements `ClusterProvisioner` + `RESTConfigProvisioner` for AKS |
| [`ClusterProvisioner Create`](#clusterprovisioner-create) | method | cluster.go | Idempotently creates the cluster and its node pools |
| [`ClusterProvisioner Describe`](#clusterprovisioner-describe) | method | cluster.go | Maps AKS state to `provisioner.ClusterState` |
| [`ClusterProvisioner Reconcile`](#clusterprovisioner-reconcile) | method | cluster.go | Reconciles access mode and node pool drift |
| [`ClusterProvisioner Delete`](#clusterprovisioner-delete) | method | cluster.go | Asynchronous, idempotent cluster teardown |
| [`ClusterProvisioner RESTConfig`](#clusterprovisioner-restconfig) | method | kubeauth.go | Builds a `*rest.Config` from AKS's generated kubeconfig |
| [`IdentityProvisioner`](#identityprovisioner) | struct | identity.go | Binds service accounts to managed identities via Workload Identity |
| [`IdentityProvisioner ProvisionForComponent`](#identityprovisioner-provisionforcomponent) | method | identity.go | Creates/upserts a managed identity + federated credential |
| [`IdentityProvisioner Deprovision`](#identityprovisioner-deprovision) | method | identity.go | Deletes the federated credential and managed identity |
| [`NetworkProvisioner`](#networkprovisioner) | struct | network.go | Implements `provisioner.NetworkProvisioner` for AKS |
| [`NetworkProvisioner EnsureNetwork`](#networkprovisioner-ensurenetwork) | method | ensure_network.go | Ensures resource group, then VNet/subnet (or passes through `spec.Subnets`) |
| [`NetworkProvisioner AllowEgress`](#networkprovisioner-allowegress) | method | network.go | Idempotently opens an outbound NSG rule for the status reporter |

## azure.go

#### `clusterAPI`

??? abstract "`clusterAPI` — interface"

    The AKS surface this package uses:

    - `Get`, `CreateOrUpdate`, `Delete` for the cluster itself
    - `GetAgentPool`, `ListAgentPools`, `CreateOrUpdateAgentPool` for node pools
    - `ListClusterUserCredentials` — returns the raw kubeconfig AKS generates (the
      same one `az aks get-credentials` writes to disk), already carrying CA data
      and, for an AAD-enabled cluster, an exec-plugin entry for token refresh

#### `identityAPI`

??? abstract "`identityAPI` — interface"

    Covers the user-assigned managed identity Workload Identity binds to, and the
    federated credential that scopes the binding:

    - `GetIdentity` / `CreateOrUpdateIdentity` / `DeleteIdentity`
    - `GetFederatedCredential` / `CreateOrUpdateFederatedCredential` / `DeleteFederatedCredential`

#### `networkAPI`

??? abstract "`networkAPI` — interface"

    Covers the status reporter's egress rule and, for `EnsureNetwork`, the
    VNet/subnet kubespin creates when none is supplied:

    - `ListSecurityGroups`, `GetSecurityRule`, `CreateOrUpdateSecurityRule`
    - `GetVirtualNetwork`, `CreateOrUpdateVirtualNetwork`
    - `GetSubnet`, `CreateOrUpdateSubnet`

#### `resourceGroupAPI`

??? abstract "`resourceGroupAPI` — interface"

    The prerequisite every other Azure resource in this package needs — ARM
    rejects a cluster, identity, or VNet create against a resource group that
    doesn't exist yet.

    - `GetResourceGroup(ctx, name) (bool, error)`
    - `EnsureResourceGroup(ctx, name, location) error`

#### `Clients`

??? abstract "`Clients` — struct"

    ```go
    type Clients struct {
        subscription   string
        cluster        clusterAPI
        identity       identityAPI
        network        networkAPI
        resourceGroups resourceGroupAPI
        logger         *slog.Logger
    }
    ```

    - Bundles the Azure clients the provisioner uses, scoped to one subscription
      fixed at construction — operator configuration, not cluster desired state,
      the same way AWS's `Clients` fixes a region and GCP's fixes a project
    - Built via `NewClients`; `realCluster`, `realIdentity`, `realNetwork`,
      `realResourceGroups` are its production adapters over the real
      `armcontainerservice`/`armmsi`/`armnetwork`/`armresources` SDK clients

    **Invariant:** Azure's control-plane SDK returns a poller for long-running
    operations (cluster create, node pool create, NSG rule create). This package
    never waits on it — `CreateOrUpdate`/`Delete`/`BeginCreateOrUpdate` calls only
    report whether the request was *accepted*, matching AWS's `CreateCluster` and
    GKE's `CreateCluster`; callers poll `Describe` afterward. Exception:
    `UserAssignedIdentitiesClient` and `FederatedIdentityCredentialsClient` calls
    are synchronous — `realIdentity` does not discard a poller because there
    isn't one. Resource group `CreateOrUpdate` is likewise synchronous.

#### `Option`

??? abstract "`Option` — func type"

    ```go
    type Option func(*Clients)
    ```

    - Configures `Clients`
    - `WithLogger(logger *slog.Logger) Option` sets the logger every provisioner
      built over the `Clients` logs through, defaulting to `slog.Default()`

#### `names`

??? abstract "`names` — struct"

    ```go
    type names struct {
        spec core.ClusterSpec
    }
    ```

    Derives every Azure resource name from the cluster ID so a cluster's
    resources are identifiable and a second cluster cannot collide with them:

    - `resourceGroup()` → `kubespin-<id>`
    - `cluster()` → `<id>`
    - `identity(comp)` → `kubespin-<id>-<comp>`
    - `federatedCredential(comp)` → `<comp>`
    - `securityRule()` → `kubespin-<id>-egress`
    - `vnet()` → `kubespin-<id>-vnet`
    - `subnet()` → `kubespin-<id>-subnet`

#### `NewClients`

??? note "`NewClients` — Signature"

    ```go
    func NewClients(subscription string, opts ...Option) (*Clients, error)
    ```

    - Builds real Azure clients for a subscription, authenticating with
      `azidentity.NewDefaultAzureCredential` (managed identity in-cluster, `az`
      CLI locally, environment variables in CI)
    - Returns an error if `subscription` is empty or if building any of the
      underlying SDK clients (AKS, agent pools, managed identities, federated
      credentials, NSGs, NSG rules, VNets, subnets, resource groups) fails

#### Helper functions

??? note "Helper functions — Signature"

    - `code(err) int` — extracts an HTTP status code from an `*azcore.ResponseError`
      (0 if not one); used throughout for 404/409 checks
    - `tags(spec) map[string]*string` — builds the `ManagedBy`/`kubespin-cluster`/
      `kubespin-profile` tag set applied to every Azure resource this package creates
    - `ptr`, `deref`, `derefInt32`, `ptrSlice`, `ptrMap`, `derefMap` — convert
      between kubespin's plain core types and the Azure SDK's `*T`-everywhere
      convention

## cluster.go

#### `ClusterProvisioner`

??? abstract "`ClusterProvisioner` — struct"

    ```go
    type ClusterProvisioner struct {
        c *Clients
    }
    ```

    - Implements `provisioner.ClusterProvisioner` and `provisioner.RESTConfigProvisioner` for AKS
    - Built via `NewClusterProvisioner(c *Clients) *ClusterProvisioner`
    - `Provider()` returns `core.ProviderAzure`

    **Invariants:**

    - `Create` is idempotent at every step — an existing cluster or node pool is
      left alone rather than treated as an error, so a resumed run passes
      straight through to whatever is still missing
    - `Reconcile` never deletes a node pool: removing one evicts running
      workloads, which is a decision left to a human, not a reconcile loop.
      Changing `vmSize` on an existing agent pool is rejected outright (AKS does
      not support it), with an error telling the operator to create a
      differently-named pool instead
    - `Delete` is asynchronous like `Create`: it returns once Azure accepts the
      request, and the caller polls `Describe` (`provisioner.WaitUntilGone`). A
      cluster already gone or already deleting is convergence, not an error —
      Azure answers a second delete against a `Deleting` cluster with 409, which
      `Delete` treats the same as already-converged rather than failing
    - AKS deletes its node pools along with the cluster, so unlike EKS there is
      no separate node-pool teardown step

#### `ClusterProvisioner Create`

??? note "`ClusterProvisioner Create` — Signature"

    ```go
    func (p *ClusterProvisioner) Create(ctx context.Context, spec core.ClusterSpec) error
    ```

    - Validates the spec (`validateForAKS`), then `Describe`s the cluster
    - If absent, calls `createCluster`
    - If already active, calls `ensureNodePools` (covers the case where creation
      partially succeeded — cluster up, pools missing)
    - Otherwise (still creating/updating/deleting) is a no-op, letting the caller poll

    `createCluster` builds a `armcontainerservice.ManagedCluster` with:

    - System-assigned identity (`ResourceIdentityTypeSystemAssigned`)
    - `APIServerAccessProfile.EnablePrivateCluster` set from
      `spec.Access == core.AccessPrivate`, and `AuthorizedIPRanges` from
      `spec.AuthorizedCIDRs`
    - `OidcIssuerProfile.Enabled = true` and
      `SecurityProfile.WorkloadIdentity.Enabled = true`
    - `NetworkProfile` with `NetworkPluginAzure`, a fixed `ServiceCidr` of
      `172.16.0.0/16`, and `DNSServiceIP` of `172.16.0.10` — disjoint from the
      `10.0.0.0/16` VNet / `10.0.1.0/24` subnet `EnsureNetwork` creates by
      default, since AKS otherwise defaults the service CIDR to a colliding
      `10.0.0.0/16`
    - The first entry of `spec.NodePools` as the initial agent pool profile
      (subsequent pools are added by `ensureNodePools`/`Reconcile`), with the
      pool's subnet set from `spec.Subnets[0]` when present

#### `ClusterProvisioner Describe`

??? note "`ClusterProvisioner Describe` — Signature"

    ```go
    func (p *ClusterProvisioner) Describe(ctx context.Context, spec core.ClusterSpec) (provisioner.ClusterState, error)
    ```

    - Returns `provisioner.StatusAbsent` (not an error) on a 404
    - Maps AKS `ProvisioningState` via `normaliseStatus` (`Succeeded`→Active,
      `Creating`→Creating, `Updating`/`Upgrading`/`Scaling`→Updating,
      `Deleting`→Deleting, `Failed`/`Canceled`→Failed, anything else→Creating)
    - Populates `Endpoint` from `Fqdn` or, for private clusters, `PrivateFQDN`;
      `OIDCIssuer` from `OidcIssuerProfile.IssuerURL`; `NetworkID` from
      `NodeResourceGroup` (where AKS places the cluster's NSG — the scope
      `AllowEgress` provisions against)
    - Node pools are only listed (`describeNodePools`) once the cluster is Active

#### `ClusterProvisioner Reconcile`

??? note "`ClusterProvisioner Reconcile` — Signature"

    ```go
    func (p *ClusterProvisioner) Reconcile(ctx context.Context, spec core.ClusterSpec) (provisioner.Change, error)
    ```

    - Errors with `provisioner.ErrNotFound` if the cluster is absent
    - Otherwise merges two changes: `reconcileAccess` (updates
      `APIServerAccessProfile` only if `state.Access != spec.Access`) and
      `ensureNodePools` (creates missing pools, resizes drifted ones by
      min/max/desired count, rejects an instance-type change)

#### `ClusterProvisioner Delete`

??? note "`ClusterProvisioner Delete` — Signature"

    ```go
    func (p *ClusterProvisioner) Delete(ctx context.Context, spec core.ClusterSpec) error
    ```

    - No-ops if the cluster is already absent or already deleting (`alreadyGoing`)
    - Calls the AKS delete API; treats a 404 as success and a 409 (lost a race
      with another teardown) as success if the cluster now shows deleting/absent

## kubeauth.go

#### `ClusterProvisioner RESTConfig`

??? note "`ClusterProvisioner RESTConfig` — Signature"

    ```go
    func (p *ClusterProvisioner) RESTConfig(ctx context.Context, spec core.ClusterSpec) (*rest.Config, error)
    ```

    - Satisfies `provisioner.RESTConfigProvisioner`
    - Requires the cluster to be Active
    - Unlike AWS/GCP (which mint a bearer token against a separately discovered
      endpoint and CA), calls `ListClusterUserCredentials` to fetch a complete
      kubeconfig — endpoint, CA data, and auth (including the exec-plugin entry
      an AAD-enabled cluster needs for token refresh) — and parses it directly
      with `clientcmd.RESTConfigFromKubeConfig`, the same thing
      `az aks get-credentials` does

## identity.go

#### `IdentityProvisioner`

??? abstract "`IdentityProvisioner` — struct"

    ```go
    type IdentityProvisioner struct {
        c *Clients
    }
    ```

    - Implements `provisioner.IdentityProvisioner`, binding in-cluster service
      accounts to Azure managed identities via Workload Identity federated
      credentials — the same "prove identity, not grant access" pattern IRSA and
      GCP Workload Identity use elsewhere in kubespin
    - Built via `NewIdentityProvisioner(c *Clients) *IdentityProvisioner`
    - `Provider()` returns `core.ProviderAzure`

    **Invariant:** the managed identity carries no role assignment. It exists so
    a component (e.g. `fleet-status-reporter`) can *prove* which cluster it is
    when it pushes status; granting it Azure access is a separate, deliberate
    decision this type does not make.

#### `IdentityProvisioner ProvisionForComponent`

??? note "`IdentityProvisioner ProvisionForComponent` — Signature"

    ```go
    func (p *IdentityProvisioner) ProvisionForComponent(
        ctx context.Context, spec core.ClusterSpec, comp provisioner.Component,
    ) (provisioner.Binding, error)
    ```

    - Describes the cluster first; errors with `provisioner.ErrNotFound` unless
      it is Active, and errors if it reports no OIDC issuer (identity binding is
      its own phase because the issuer only exists once the control plane is up)
    - Then:
        1. `ensureIdentity` — gets or creates a `armmsi.Identity` named
           `kubespin-<id>-<component>`, returning its client ID
        2. `ensureFederatedCredential` — gets or creates (upserts on drift) a
           federated credential named after the component, with `Issuer` = the
           cluster's OIDC issuer, `Subject` =
           `system:serviceaccount:<namespace>:<serviceAccount>`, and `Audiences`
           = `["api://AzureADTokenExchange"]`. If an existing credential already
           matches issuer and subject, it's left alone (no-op)
    - Returns a `provisioner.Binding` with `Identifier` = the identity's client
      ID and `Annotations["azure.workload.identity/client-id"]` set — the
      annotation key the caller applies blind, not knowing which cloud it's on

#### `IdentityProvisioner Deprovision`

??? note "`IdentityProvisioner Deprovision` — Signature"

    ```go
    func (p *IdentityProvisioner) Deprovision(ctx context.Context, spec core.ClusterSpec, comp provisioner.Component) error
    ```

    - Deletes the federated credential, then the managed identity
    - Both deletes treat a 404 as success, so a retried teardown converges
      rather than failing

## network.go

#### `NetworkProvisioner`

??? abstract "`NetworkProvisioner` — struct"

    ```go
    type NetworkProvisioner struct {
        c       *Clients
        cluster *ClusterProvisioner
    }
    ```

    - Implements `provisioner.NetworkProvisioner`
    - Built via `NewNetworkProvisioner(c *Clients) *NetworkProvisioner`, which
      also constructs an internal `ClusterProvisioner` used to look up the
      cluster's node resource group
    - `Provider()` returns `core.ProviderAzure`

#### `NetworkProvisioner AllowEgress`

??? note "`NetworkProvisioner AllowEgress` — Signature"

    ```go
    func (p *NetworkProvisioner) AllowEgress(
        ctx context.Context, spec core.ClusterSpec, dest provisioner.EgressDestination,
    ) (provisioner.Change, error)
    ```

    - The only route fleet state has out of a cluster
    - Describes the cluster to get `NetworkID` (the node resource group) —
      errors with `provisioner.ErrNotFound` if not yet set
    - Looks up the NSG within that resource group via `findSecurityGroup` (AKS
      places the NSG there under a name it controls, so this looks it up rather
      than assuming a fixed name; errors with `provisioner.ErrNotFound` if none
      exists yet)
    - If a security rule named `n.securityRule()` already exists, it's left
      alone (idempotent — no duplicate rules on a repeated apply)
    - Otherwise creates an outbound TCP allow rule at priority 200, defaulting
      `dest.CIDR` to `0.0.0.0/0`, `dest.Port` to `443`, and `dest.Description`
      to `"kubespin fleet-status-reporter egress"` when unset

## ensure_network.go

#### `NetworkProvisioner EnsureNetwork`

??? note "`NetworkProvisioner EnsureNetwork` — Signature"

    ```go
    func (p *NetworkProvisioner) EnsureNetwork(
        ctx context.Context, spec core.ClusterSpec,
    ) (provisioner.NetworkResult, error)
    ```

    - Unconditionally ensures the resource group exists first
      (`GetResourceGroup` / `EnsureResourceGroup`), since it is a prerequisite
      for the cluster and for any network this package creates
    - If `spec.Subnets` is already set, returns it unchanged (`Change` stays
      zero-value / unchanged) — the operator owns that network and kubespin
      does not touch it
    - If empty:
        - Uses `spec.VNetCIDR`/`spec.SubnetCIDR` if set, else defaults
          `10.0.0.0/16` / `10.0.1.0/24` (`defaultVNetCIDR`/`defaultSubnetCIDR`)
        - Creates the VNet (`n.vnet()`) if it doesn't already exist (404 check first)
        - Delegates to `ensureSubnet`, which similarly checks-then-creates the
          subnet (`n.subnet()`), and returns the subnet's Azure resource ID
    - Returns `NetworkResult{SubnetIDs: [subnetID], Change: change}`, where
      `change` records whether the VNet and/or subnet were newly created, so a
      resumed or repeated `apply` adopts existing resources instead of
      duplicating them
