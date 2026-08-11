# internal/provisioner (shared interfaces) and internal/provisioner/aws

Reference for the shared cloud-provisioning interfaces in
[`internal/provisioner/provisioner.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/provisioner/provisioner.go)
and the EKS implementation in `internal/provisioner/aws/` (`aws.go`,
`cluster.go`, `identity.go`, `kubeauth.go`, `network.go`). GCP and Azure
implement the same interfaces from behind their own subpackages; nothing
here is cloud-specific to that pair.

## Shared interfaces (`provisioner.go`)

### Sentinel errors and types

```go
var (
    ErrNotFound      = errors.New("cluster does not exist")
    ErrUnsupported   = errors.New("unsupported by this provider")
    ErrClusterFailed = errors.New("cluster is in a failed state")
)

type Status string

const (
    StatusAbsent   Status = "absent"
    StatusCreating Status = "creating"
    StatusActive   Status = "active"
    StatusUpdating Status = "updating"
    StatusDeleting Status = "deleting"
    StatusFailed   Status = "failed"
)

func (s Status) Settled() bool // true for StatusActive, StatusFailed, StatusAbsent
```

`ClusterState` is what the cloud currently reports:

```go
type ClusterState struct {
    Status                   Status
    Endpoint                 string
    OIDCIssuer               string       // populated once active; identity binding is a separate phase
    Version                  string
    Access                   core.Access
    NodePools                []core.NodePool
    NetworkID                string       // AWS: cluster security group; GCP: network; Azure: NSG
    CertificateAuthorityData []byte       // already base64-decoded
}
```

`Change` is the outcome of a `Reconcile`/`EnsureNetwork`/`AllowEgress` call — reported as data rather than inferred by diffing before/after state, because `apply` must be able to prove it made zero cloud calls when nothing differs:

```go
type Change struct {
    Changed bool
    Details []string
}

func (c *Change) Merge(other Change)
```

### `ClusterProvisioner`

Manages a cluster's lifecycle on one cloud. `Create` is asynchronous on every cloud (provisioning takes 10–30 minutes), so it returns as soon as the request is accepted; callers poll `Describe`.

```go
type ClusterProvisioner interface {
    Provider() core.Provider
    Create(ctx context.Context, spec core.ClusterSpec) error
    Describe(ctx context.Context, spec core.ClusterSpec) (ClusterState, error)
    Reconcile(ctx context.Context, spec core.ClusterSpec) (Change, error)
    Delete(ctx context.Context, spec core.ClusterSpec) error
}
```

Contract each implementation must honor:

- `Create` is idempotent — creating a cluster that already exists is a no-op.
- `Describe` returns `StatusAbsent` (not an error) when the cluster does not exist, since "not there yet" is a normal polling answer.
- `Reconcile` brings node pool sizing and access configuration in line with the spec, reporting via `Change` whether anything changed.
- `Delete` is idempotent — deleting an absent cluster is a no-op, so a retried teardown converges.

### `IdentityProvisioner`

Binds a cloud-native workload identity to an in-cluster service account. The identity exists to be *proven*, not to grant cloud access — `Component` carries no permission set.

```go
type Component struct {
    Name           string
    Namespace      string
    ServiceAccount string
}

type Binding struct {
    Identifier  string            // IAM role ARN / GCP service account email / Azure client ID
    Annotations map[string]string // applied to the Kubernetes ServiceAccount, key differs per cloud
}

type IdentityProvisioner interface {
    Provider() core.Provider
    ProvisionForComponent(ctx context.Context, spec core.ClusterSpec, comp Component) (Binding, error)
    Deprovision(ctx context.Context, spec core.ClusterSpec, comp Component) error
}
```

`ProvisionForComponent` is idempotent, returning the existing binding when one is already in place. `Deprovision` removes the identity and is a no-op if it is already absent. `StatusReporter()` returns the one `Component` every cluster provisions: `fleet-status-reporter` in namespace `kubespin-system`, service account `fleet-status-reporter`.

### `NetworkProvisioner`

Opens the one outbound path the architecture depends on and resolves the network a cluster is created in.

```go
type EgressDestination struct {
    Host        string
    Port        int32
    CIDR        string
    Description string
}

type NetworkResult struct {
    SubnetIDs []string
    Change    Change
}

type NetworkProvisioner interface {
    Provider() core.Provider
    EnsureNetwork(ctx context.Context, spec core.ClusterSpec) (NetworkResult, error)
    AllowEgress(ctx context.Context, spec core.ClusterSpec, dest EgressDestination) (Change, error)
}
```

Contract: if `spec.Subnets` is already set, `EnsureNetwork` passes it through unchanged (`Change.Changed` stays `false`) — kubespin never touches a network an operator already supplied. If empty, every implementation creates a network deterministically named from the cluster ID and adopts it on a repeated call, so a resumed or repeated `apply` converges rather than duplicating resources.

### `RESTConfigProvisioner`

Builds a Kubernetes `*rest.Config` for a cluster this cloud created, so the Argo CD installer can reach it without storing any credential — the bearer token is minted fresh from the same cloud-native identity `kubespin login` already established.

```go
type RESTConfigProvisioner interface {
    RESTConfig(ctx context.Context, spec core.ClusterSpec) (*rest.Config, error)
}
```

The cluster must be active; the endpoint and CA data come from the same `Describe` call every other caller uses.

### Polling helpers

`WaitOptions` tunes `WaitUntilActive`/`WaitUntilGone`:

```go
type WaitOptions struct {
    Interval          time.Duration
    Timeout           time.Duration
    MaxDescribeErrors int          // consecutive failed Describe calls tolerated before giving up; 0 = DefaultMaxDescribeErrors
    Logger            *slog.Logger
}

const DefaultMaxDescribeErrors = 5

func DefaultWaitOptions() WaitOptions // Interval 30s, Timeout 45m, MaxDescribeErrors 5
```

```go
func WaitUntilActive(ctx context.Context, p ClusterProvisioner, spec core.ClusterSpec, opts WaitOptions) (ClusterState, error)
func WaitUntilGone(ctx context.Context, p ClusterProvisioner, spec core.ClusterSpec, opts WaitOptions) error
```

Both poll `Describe` on `opts.Interval` until the cluster settles (`WaitUntilActive`) or disappears (`WaitUntilGone`), tolerating up to `MaxDescribeErrors` consecutive `Describe` failures as transient before failing the wait — polling a control plane for up to 45 minutes means hundreds of API calls against a cloud that throttles or occasionally drops a connection, so a single blip must not throw away an otherwise-successful creation.

## AWS implementation (`internal/provisioner/aws`)

Package doc: "provisions EKS clusters and IRSA identities." Every AWS service is reached through a narrow interface listing only the calls the package makes — this keeps the provisioner testable without credentials and doubles as the exact IAM permission set an operator must grant.

### `aws.go` — clients and naming

#### `eksAPI`, `iamAPI`, `ec2API` interfaces

Narrow interfaces over the AWS SDK v2 clients:

- `eksAPI`: `DescribeCluster`, `CreateCluster`, `UpdateClusterConfig`, `DeleteCluster`, `ListNodegroups`, `DescribeNodegroup`, `CreateNodegroup`, `UpdateNodegroupConfig`, `DeleteNodegroup`.
- `iamAPI`: service-role and IRSA-role calls — `GetRole`, `CreateRole`, `DeleteRole`, `UpdateAssumeRolePolicy`, `AttachRolePolicy`, `ListAttachedRolePolicies`, `DetachRolePolicy`, `ListOpenIDConnectProviders`, `GetOpenIDConnectProvider`, `CreateOpenIDConnectProvider`.
- `ec2API`: the status reporter's egress rule (`DescribeSecurityGroupRules`, `AuthorizeSecurityGroupEgress`) plus, when `spec.Subnets` is empty, VPC/subnet/IGW/route-table creation (`DescribeVpcs`, `CreateVpc`, `ModifyVpcAttribute`, `DescribeAvailabilityZones`, `DescribeSubnets`, `CreateSubnet`, `DescribeInternetGateways`, `CreateInternetGateway`, `AttachInternetGateway`, `DescribeRouteTables`, `CreateRouteTable`, `CreateRoute`, `AssociateRouteTable`).

### `Clients`

```go
type Clients struct {
    eks eksAPI
    iam iamAPI
    ec2 ec2API
    sts stsPresignAPI
    logger *slog.Logger
}

type Option func(*Clients)

func WithLogger(logger *slog.Logger) Option
func NewClients(ctx context.Context, region string, opts ...Option) (*Clients, error)
```

`NewClients` loads the default AWS config for `region` (`config.LoadDefaultConfig`) and builds real `eks`, `iam`, `ec2` clients plus an STS presign client. Every provisioner type below (`ClusterProvisioner`, `IdentityProvisioner`, `NetworkProvisioner`) wraps a shared `*Clients`.

### AWS-managed policy / OIDC constants

```go
const (
    policyEKSCluster        = "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"
    policyEKSWorkerNode     = "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy"
    policyEKSCNI            = "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy"
    policyECRReadOnly       = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"
    eksOIDCThumbprint       = "9e99a48a9960b14926bb7f3b02e22da2b0ab7280"
    eksOIDCClientIDAudience = "sts.amazonaws.com"
)
```

AWS-managed policies are attached rather than authored, so the cluster stays current as AWS extends what EKS control planes and nodes need.

### `names` — deterministic resource naming

```go
type names struct{ spec core.ClusterSpec }

func (n names) cluster() string
func (n names) clusterRole() string             // "kubespin-<clusterID>-cluster"
func (n names) nodeRole() string                // "kubespin-<clusterID>-node"
func (n names) nodeGroup(pool string) string     // "<clusterID>-<pool>"
func (n names) irsaRole(comp string) string      // "kubespin-<clusterID>-<comp>"
func (n names) vpcName() string                  // "kubespin-<clusterID>"
func (n names) subnetName(az string) string      // "kubespin-<clusterID>-subnet-<az>"
func (n names) igwName() string                  // "kubespin-<clusterID>-igw"
func (n names) routeTableName() string           // "kubespin-<clusterID>-rt"
```

Every AWS resource kubespin creates is name-derived from the cluster ID, so resources are identifiable and a second cluster cannot collide with them; this same deterministic naming is what lets `EnsureNetwork` and `ensureRole` adopt existing resources on a resumed `apply` instead of duplicating them.

`func tags(spec core.ClusterSpec) map[string]string` returns the common tag set applied to every AWS resource: `ManagedBy: kubespin`, `kubespin/cluster: <ID>`, `kubespin/profile: <profile>`.

### `cluster.go` — EKS cluster and node group lifecycle

#### `ClusterProvisioner`

```go
type ClusterProvisioner struct {
    c    *Clients
    wait provisioner.WaitOptions // tunes the polls Delete makes while node groups drain
}

func NewClusterProvisioner(c *Clients) *ClusterProvisioner
func (p *ClusterProvisioner) Provider() core.Provider // core.ProviderAWS
```

Implements `provisioner.ClusterProvisioner` and (via `kubeauth.go`) `provisioner.RESTConfigProvisioner`.

**`Create(ctx, spec) error`** — validates the spec (`validateForEKS`), ensures the EKS cluster service role exists with `AmazonEKSClusterPolicy` attached, then `Describe`s the cluster. If absent, it calls `createCluster` and returns — node groups cannot attach until the control plane is active, so they are deferred to `Reconcile` once the caller has polled to active. If already active, it calls `ensureNodeGroups` directly (covers a resumed run that crashed after cluster creation but before node groups).

`createCluster` issues `eks.CreateCluster` with:

```go
&eks.CreateClusterInput{
    Name:               names{spec}.cluster(),
    RoleArn:            clusterRoleARN,
    ResourcesVpcConfig: vpcConfig(spec),
    Tags:               tags(spec),
    Version:            spec.KubernetesVersion, // only if non-empty
}
```

`ekstypes.ResourceInUseException` from `CreateCluster` is treated as convergence (another run got there first), not failure.

**`vpcConfig(spec) *ekstypes.VpcConfigRequest`** — translates access mode into EKS endpoint config:

```go
func vpcConfig(spec core.ClusterSpec) *ekstypes.VpcConfigRequest {
    cfg := &ekstypes.VpcConfigRequest{
        SubnetIds:             spec.Subnets,
        EndpointPrivateAccess: aws.Bool(true),
        EndpointPublicAccess:  aws.Bool(spec.Access == core.AccessPublic),
    }
    if spec.Access == core.AccessPublic && len(spec.AuthorizedCIDRs) > 0 {
        cfg.PublicAccessCidrs = spec.AuthorizedCIDRs
    }
    return cfg
}
```

A private cluster (`Access: private`) has no public endpoint at all; a public cluster is reachable but restricted to `AuthorizedCIDRs` when any are given. Both keep the private endpoint enabled so in-VPC traffic never leaves the network. This is the same function `reconcileAccess` calls to flip access mode later, so creation and reconciliation share one source of truth for endpoint config.

**`Describe(ctx, spec) (provisioner.ClusterState, error)`** — calls `eks.DescribeCluster`; a `ekstypes.ResourceNotFoundException` maps to `StatusAbsent` with no error. Otherwise builds `ClusterState`: `Status` via `normaliseStatus`, `Endpoint`, `Version`, `Access` via `accessFrom(cluster.ResourcesVpcConfig)`, `OIDCIssuer` from `cluster.Identity.Oidc.Issuer`, `NetworkID` from `cluster.ResourcesVpcConfig.ClusterSecurityGroupId`, and `CertificateAuthorityData` base64-decoded from `cluster.CertificateAuthority.Data`. When the cluster is active, it also populates `NodePools` via `describeNodePools`.

`normaliseStatus` maps EKS's `ClusterStatus` to the shared `provisioner.Status`: `Active→Active`, `Creating→Creating`, `Updating→Updating`, `Deleting→Deleting`, `Pending→Creating` (deliberately grouped with waiting, not failure — EKS reports Pending for a cluster that has not started yet and it clears on its own), `Failed`/anything else `→Failed`.

`accessFrom(cfg)` returns `core.AccessPublic` if `cfg.EndpointPublicAccess` is true, else `core.AccessPrivate`.

`describeNodePools` lists node groups (`ListNodegroups`) then `DescribeNodegroup`s each, mapping to `core.NodePool{Name, Labels, DiskSizeGB, InstanceType (first entry), MinSize, MaxSize, DesiredSize}`. `poolNameFromNodeGroup` strips the `<clusterID>-` prefix EKS's node group name carries to recover the pool name from `spec.NodePools`. Results are sorted by name for deterministic output.

**`Reconcile(ctx, spec) (provisioner.Change, error)`** — `Describe`s the cluster (errors if `StatusAbsent`, wrapping `provisioner.ErrNotFound`), then merges the `Change` from `reconcileAccess` and `ensureNodeGroups`.

`reconcileAccess` compares `state.Access` to `spec.Access`; if they differ it calls `eks.UpdateClusterConfig` with the same `vpcConfig(spec)` used at creation, and reports a `Change` detail `"access <old> -> <new>"`.

`ensureNodeGroups` first ensures the node IAM role (`AmazonEKSWorkerNodePolicy`, `AmazonEKS_CNI_Policy`, `AmazonEC2ContainerRegistryReadOnly`) exists. For each pool in `spec.NodePools`: if missing, calls `createNodeGroup`; if present and sizing (`MinSize`/`MaxSize`/`DesiredSize`) differs, calls `eks.UpdateNodegroupConfig` to resize. **It never deletes a node group** — removing a pool would evict running workloads, a decision reserved for a human. Each create/resize is recorded into the `*provisioner.Change` via the `record` helper.

`createNodeGroup` calls `eks.CreateNodegroup` with subnets from `spec.Subnets`, the pool's single `InstanceType`, scaling config, labels, and tags; `ekstypes.ResourceInUseException` is treated as convergence.

**`Delete(ctx, spec) error`** — `Describe`s first; a no-op if already `StatusAbsent` or `StatusDeleting`. Lists node groups and deletes each (`DeleteNodegroup`) — node groups must go first because EKS refuses to delete a cluster with any attached — then calls `waitForNodeGroupsGone` if any existed, then `eks.DeleteCluster`. `ResourceNotFoundException` at any step converges rather than erroring, so a retried teardown resumes cleanly.

`waitForNodeGroupsGone` polls `ListNodegroups` on `p.wait.Interval`/`p.wait.Timeout` (falling back to `provisioner.DefaultWaitOptions()` values if unset) until the list is empty, because `DeleteNodegroup` only accepts the request — draining and terminating nodes takes minutes, and `DeleteCluster` fails with `ResourceInUseException` the whole time.

#### Role helpers

`ensureRole(ctx, name, trust, policies) (string, error)` — `GetRole`; if `NoSuchEntityException`, marshals `trust` to JSON and `CreateRole`s it. Either way, calls `attachPolicies` to reconcile the policy set, and returns the role ARN.

`attachPolicies(ctx, role, policies) error` — lists currently attached policies and attaches any from `policies` not already present (never detaches).

`eksServiceTrust(service string) map[string]any` — builds a trust policy allowing `sts:AssumeRole` for the given AWS service principal (`eks.amazonaws.com` for the cluster role, `ec2.amazonaws.com` for the node role).

#### Validation and misc

```go
func validateForEKS(spec core.ClusterSpec) error
```

Rejects specs with fewer than 2 subnets — EKS places the control plane's cross-account ENIs in at least two Availability Zones and rejects fewer at creation time. Wraps `core.ErrInvalidSpec`.

```go
func findPool(pools []core.NodePool, name string) (core.NodePool, bool)
func record(change *provisioner.Change, detail string) // no-op if change is nil (Create passes nil — creation isn't a reconcile finding)
```

### `identity.go` — IRSA

#### `IdentityProvisioner`

```go
type IdentityProvisioner struct {
    c       *Clients
    cluster *ClusterProvisioner
}

func NewIdentityProvisioner(c *Clients) *IdentityProvisioner
func (p *IdentityProvisioner) Provider() core.Provider // core.ProviderAWS
```

**`ProvisionForComponent(ctx, spec, comp) (provisioner.Binding, error)`** — `Describe`s the cluster via the embedded `ClusterProvisioner`; errors (wrapping `provisioner.ErrNotFound`) unless the cluster is `StatusActive` with a non-empty `OIDCIssuer` — the issuer only exists once the control plane is up, which is why identity binding is its own orchestrator phase rather than part of cluster creation. Then calls `ensureOIDCProvider` and `ensureIRSARole`, returning:

```go
provisioner.Binding{
    Identifier:  roleARN,
    Annotations: map[string]string{"eks.amazonaws.com/role-arn": roleARN},
}
```

**`ensureOIDCProvider(ctx, issuer) (string, error)`** — lists existing IAM OIDC providers (`ListOpenIDConnectProviders`), `GetOpenIDConnectProvider`s each to compare its `Url` against the issuer host, and reuses a match. If none, calls `CreateOpenIDConnectProvider` with `ClientIDList: [sts.amazonaws.com]` and the fixed thumbprint `eksOIDCThumbprint`. On `EntityAlreadyExistsException` (a concurrent run registered it between the list and this call), it falls back to `findOIDCProvider` to look the ARN up again rather than failing.

**`ensureIRSARole(ctx, spec, comp, providerARN, issuer) (string, error)`** — role name `names{spec}.irsaRole(comp.Name)`. If the role exists, its trust policy is **unconditionally rewritten** (`UpdateAssumeRolePolicy`) rather than compared — the trust policy is the only thing standing between this role and any other service account in the cluster, so drift in it is a privilege-escalation risk, not merely staleness. If missing, `CreateRole` with the trust document.

**`irsaTrustPolicy(providerARN, issuer, comp) map[string]any`** — scopes the role to exactly one service account in one namespace of one cluster via `sts:AssumeRoleWithWebIdentity` with two `StringEquals` conditions on the OIDC host:

```go
"<host>:sub" == "system:serviceaccount:<namespace>:<serviceaccount>"
"<host>:aud" == "sts.amazonaws.com"
```

Both conditions matter: without `sub` any service account in the cluster could assume the role; without `aud` a token minted for another audience would be accepted.

**`Deprovision(ctx, spec, comp) error`** — lists and detaches all policies on the component's IRSA role (IAM refuses to delete a role with attached policies), then `DeleteRole`. `NoSuchEntityException` at any step is treated as already-gone. The cluster's OIDC provider is deliberately left in place — it belongs to the cluster, not to one component, and other components may still depend on it; cluster teardown removes it as part of the cluster, not here.

### `kubeauth.go` — REST config / bearer token minting

```go
const eksTokenPrefix = "k8s-aws-v1."

type stsPresignAPI interface {
    PresignGetCallerIdentityURL(ctx context.Context, clusterName string) (string, error)
}

type stsPresigner struct{ client *sts.PresignClient }

func newSTSPresigner(cfg aws.Config) *stsPresigner
func (p *stsPresigner) PresignGetCallerIdentityURL(ctx context.Context, clusterName string) (string, error)
```

`PresignGetCallerIdentityURL` presigns an STS `GetCallerIdentity` request and injects an `x-k8s-aws-id: <clusterName>` header via a Smithy build middleware — this is what `aws-iam-authenticator` (built into every EKS control plane) checks to scope the token to one cluster; a token presigned for a different cluster name is rejected.

```go
func (p *ClusterProvisioner) RESTConfig(ctx context.Context, spec core.ClusterSpec) (*rest.Config, error)
```

Satisfies `provisioner.RESTConfigProvisioner`. `Describe`s the cluster (must be `StatusActive`), mints a presigned URL via `p.c.sts.PresignGetCallerIdentityURL`, and returns:

```go
&rest.Config{
    Host:            state.Endpoint,
    BearerToken:     "k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte(url)),
    TLSClientConfig: rest.TLSClientConfig{CAData: state.CertificateAuthorityData},
}
```

No static credential is ever written down — the token is derived fresh from whatever session `kubespin login` cached, matching the format `aws eks get-token` produces, and its lifetime is bounded by the presigned URL's default 60-second `X-Amz-Expires`.

### `network.go` — VPC/subnet auto-creation and egress

```go
const (
    defaultVPCCIDR  = "10.0.0.0/16"
    subnetPrefixLen = 24 // each carved subnet
    subnetsWanted   = 2  // EKS control plane minimum
)
```

#### `NetworkProvisioner`

```go
type NetworkProvisioner struct {
    c       *Clients
    cluster *ClusterProvisioner
}

func NewNetworkProvisioner(c *Clients) *NetworkProvisioner
func (p *NetworkProvisioner) Provider() core.Provider // core.ProviderAWS
```

**`EnsureNetwork(ctx, spec) (provisioner.NetworkResult, error)`** — if `spec.Subnets` is already set, returns it unchanged with no `Change`. Otherwise, using `vpcCIDR` (`spec.VPCCIDR` or `defaultVPCCIDR`):

1. `ensureVPC` — looks up a VPC tagged `Name: kubespin-<clusterID>`; creates one if absent (`CreateVpc`), then enables DNS support and DNS hostnames via two `ModifyVpcAttribute` calls (EKS requires both; neither defaults on for a new VPC).
2. `availabilityZones` — lists available AZs in the region (`DescribeAvailabilityZones`, filtered to `state=available`), sorted alphabetically so the pair chosen is deterministic across runs. Errors if the region has fewer than `subnetsWanted` (2) AZs.
3. For `i in [0, 1]`: `carveSubnetCIDR(vpcCIDR, i)` computes the i-th `/24` block of the VPC CIDR, then `ensureSubnet` looks up (by `Name: kubespin-<clusterID>-subnet-<az>` tag) or creates (`CreateSubnet`) a subnet in `azs[i]` with that CIDR.
4. `ensureInternetGateway` — looks up or creates (`CreateInternetGateway` + `AttachInternetGateway`) an IGW tagged `kubespin-<clusterID>-igw`.
5. `ensureRouteTable` — looks up a route table tagged `kubespin-<clusterID>-rt`; if none exists, creates one, adds a default route (`0.0.0.0/0` via the IGW), and associates both subnets with it. **A single shared public route table is used for both subnets** — kubespin does not split into private subnets behind a NAT gateway; that is out of scope (expensive to run/test, and the architecture only requires nothing reach *in*, not that nodes be unreachable from their own VPC's egress path).

Returns `NetworkResult{SubnetIDs: [2 subnet IDs], Change}`. Every step is create-or-adopt, keyed off the deterministic `Name` tag from `names{spec}`, so a resumed or repeated `apply` converges onto the same VPC/subnets/IGW/route table rather than duplicating them.

`carveSubnetCIDR(vpcCIDR, index) (string, error)` — parses `vpcCIDR` as IPv4, requires it to be at least a `/24` (errors otherwise), and computes the `index`-th `/24` block by adding `index * 256` to the base IP as a big-endian uint32.

`tagNameFilter(name)` / `tagSpec(resourceType, name, spec)` — build the EC2 `Name`-tag filter used for lookups, and the tag specification (`Name` + the common `tags(spec)` set) applied on creation, respectively.

**`AllowEgress(ctx, spec, dest) (provisioner.Change, error)`** — `Describe`s the cluster to get `state.NetworkID` (the cluster security group ID); errors (`provisioner.ErrNotFound`) if empty. Defaults `dest.CIDR` to `0.0.0.0/0` and `dest.Port` to `443` if unset. Checks `egressRuleExists`; if a matching rule is already present, returns a no-op `Change`. Otherwise calls `AuthorizeSecurityGroupEgress` for TCP on the resolved port/CIDR with a description (defaulting to `"kubespin fleet-status-reporter egress"`).

`egressRuleExists(ctx, groupID, cidr, port) (bool, error)` — lists the security group's rules (`DescribeSecurityGroupRules`) and looks for an existing egress rule matching the CIDR: an allow-all rule (`IpProtocol: "-1"`) always counts as covering the destination; otherwise a TCP rule whose port range contains `port` counts. This idempotency is what keeps a resumed or repeated `apply` from accumulating duplicate rules.

## Access-mode summary (AWS)

| Access mode | `EndpointPrivateAccess` | `EndpointPublicAccess` | `PublicAccessCidrs` |
|---|---|---|---|
| `private` | `true` | `false` | not set |
| `public` | `true` | `true` | `spec.AuthorizedCIDRs` if non-empty, else unrestricted |

Both modes leave the private endpoint enabled so in-VPC traffic never has to leave the network. `vpcConfig` (`cluster.go`) is the single function that encodes this branching, used both at cluster creation and by `reconcileAccess` when `apply` detects `spec.Access` has changed.
