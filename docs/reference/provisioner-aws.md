# internal/provisioner (shared interfaces) and internal/provisioner/aws

Reference for the shared cloud-provisioning interfaces in
[`internal/provisioner/provisioner.go`](https://github.com/GitOpsHub/kubespin/blob/main/internal/provisioner/provisioner.go)
and the EKS implementation in `internal/provisioner/aws/` (`aws.go`,
`cluster.go`, `identity.go`, `kubeauth.go`, `network.go`). GCP and Azure
implement the same interfaces from behind their own subpackages; nothing
here is cloud-specific to that pair.

## Quick reference

### Shared interfaces (`provisioner.go`)

| Name | Kind | Summary |
|---|---|---|
| [Sentinel errors and types](#sentinel-errors-and-types) | errors/types | `ErrNotFound`, `ErrUnsupported`, `ErrClusterFailed`, `Status` enum |
| [`ClusterState`](#clusterstate) | struct | what the cloud currently reports for a cluster |
| [`Change`](#change) | struct | outcome of a `Reconcile`/`EnsureNetwork` call |
| [`ClusterProvisioner`](#clusterprovisioner) | interface | manages a cluster's lifecycle on one cloud |
| [`NetworkProvisioner`](#networkprovisioner) | interface | resolves, creates, and deletes the cluster's network |
| [`RESTConfigProvisioner`](#restconfigprovisioner) | interface | builds a `*rest.Config` for a cloud-created cluster |
| [Polling helpers](#polling-helpers) | functions | `WaitUntilActive`/`WaitUntilGone` over `Describe` |

### AWS implementation (`internal/provisioner/aws`)

| Name | Kind | File | Summary |
|---|---|---|---|
| [`eksAPI`, `iamAPI`, `ec2API`](#eksapi-iamapi-ec2api-interfaces) | interfaces | aws.go | narrow SDK v2 client interfaces |
| [`Clients`](#clients) | struct | aws.go | shared SDK clients + logger |
| [AWS-managed policy constants](#aws-managed-policy--oidc-constants) | constants | aws.go | policy ARNs |
| [`names`](#names--deterministic-resource-naming) | struct | aws.go | deterministic resource naming from cluster ID |
| [`ClusterProvisioner`](#clusterprovisioner-1) | struct | cluster.go | EKS cluster + node group lifecycle |
| [EKS add-ons](#addonsgo) | functions | addons.go | `networkAddons`, `workloadAddons`, `ensureManagedAddon` — every EKS add-on kubespin installs, with Pod Identity |
| [Cluster autoscaler identity](#autoscalergo) | functions | autoscaler.go | `ensureClusterAutoscalerIdentity` — Pod Identity for the one Helm chart that needs AWS permissions |
| [Spot instance selection](#instancetypesgo) | functions | instancetypes.go | `resolveAutoInstanceTypes` — the cheapest spot types for `instanceType: auto` |
| [Role helpers](#role-helpers) | functions | cluster.go | `ensureRole`, `attachPolicies`, `eksServiceTrust` |
| [Validation and misc](#validation-and-misc) | functions | cluster.go | `validateForEKS`, `findPool`, `record` |
| [REST config / bearer token minting](#kubeauthgo--rest-config--bearer-token-minting) | functions | kubeauth.go | STS-presigned bearer token for `*rest.Config` |
| [`NetworkProvisioner`](#networkprovisioner-1) | struct | network.go | VPC/subnet auto-creation and teardown |

## Shared interfaces (`provisioner.go`)

### Sentinel errors and types

<details>
<summary>Signature</summary>

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

</details>

### `ClusterState`

What the cloud currently reports:

<details>
<summary>Signature</summary>

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

</details>

### `Change`

The outcome of a `Reconcile`/`EnsureNetwork` call — reported as data rather than inferred by diffing before/after state, because `apply` must be able to prove it made zero cloud calls when nothing differs.

<details>
<summary>Signature</summary>

```go
type Change struct {
    Changed bool
    Details []string
}

func (c *Change) Merge(other Change)
```

</details>

### `ClusterProvisioner`

Manages a cluster's lifecycle on one cloud. `Create` is asynchronous on every cloud (provisioning takes 10–30 minutes), so it returns as soon as the request is accepted; callers poll `Describe`.

<details>
<summary>Signature</summary>

```go
type ClusterProvisioner interface {
    Provider() core.Provider
    Create(ctx context.Context, spec core.ClusterSpec) error
    Describe(ctx context.Context, spec core.ClusterSpec) (ClusterState, error)
    Reconcile(ctx context.Context, spec core.ClusterSpec) (Change, error)
    Delete(ctx context.Context, spec core.ClusterSpec) error
}
```

- **Contract:**
    - `Create` is idempotent — creating a cluster that already exists is a no-op.
    - `Describe` returns `StatusAbsent` (not an error) when the cluster does not exist, since "not there yet" is a normal polling answer.
    - `Reconcile` brings node pool sizing and access configuration in line with the spec, reporting via `Change` whether anything changed.
    - `Delete` is idempotent — deleting an absent cluster is a no-op, so a retried teardown converges.

</details>

### `NetworkProvisioner`

Resolves, creates, and deletes the network a cluster lives in.

<details>
<summary>Signature</summary>

```go
type NetworkResult struct {
    SubnetIDs []string
    Change    Change
}

type NetworkProvisioner interface {
    Provider() core.Provider
    EnsureNetwork(ctx context.Context, spec core.ClusterSpec) (NetworkResult, error)
    DeleteNetwork(ctx context.Context, spec core.ClusterSpec) error
}
```

- **Contract:**
    - If `spec.Subnets` is already set, `EnsureNetwork` passes it through unchanged (`Change.Changed` stays `false`) — kubespin never touches a network an operator already supplied.
    - If empty, every implementation creates a network deterministically named from the cluster ID and adopts it on a repeated call, so a resumed or repeated `apply` converges rather than duplicating resources.

</details>

### `RESTConfigProvisioner`

Builds a Kubernetes `*rest.Config` for a cluster this cloud created, so the Argo CD installer can reach it without storing any credential — the bearer token is minted fresh from the same cloud-native identity `kubespin login` already established.

<details>
<summary>Signature</summary>

```go
type RESTConfigProvisioner interface {
    RESTConfig(ctx context.Context, spec core.ClusterSpec) (*rest.Config, error)
}
```

- **Contract:** the cluster must be active; the endpoint and CA data come from the same `Describe` call every other caller uses.

</details>

### Polling helpers

`WaitOptions` tunes `WaitUntilActive`/`WaitUntilGone`:

<details>
<summary>Signature</summary>

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

- **Behavior:**
    - Both poll `Describe` on `opts.Interval` until the cluster settles (`WaitUntilActive`) or disappears (`WaitUntilGone`).
    - Both tolerate up to `MaxDescribeErrors` consecutive `Describe` failures as transient before failing the wait — polling a control plane for up to 45 minutes means hundreds of API calls against a cloud that throttles or occasionally drops a connection, so a single blip must not throw away an otherwise-successful creation.

</details>

## AWS implementation (`internal/provisioner/aws`)

Package doc: "provisions EKS clusters and their networks." Every AWS service is reached through a narrow interface listing only the calls the package makes — this keeps the provisioner testable without credentials and doubles as the exact IAM permission set an operator must grant.

## addons.go

Where AWS ships an EKS add-on for a component a cluster needs, kubespin installs that through the EKS add-on API instead of a Helm chart delivered by Argo CD. EKS owns the add-on's lifecycle and its compatibility with the control plane, and binds its AWS permissions itself. The catalog limits the Helm equivalents (`cert-manager`, `external-dns`, `fluent-bit`) to GCP/Azure through `Providers`, so the two never both run on one cluster.

| Add-on | Phase | Pod Identity role | Configuration |
|---|---|---|---|
| `vpc-cni` | before node groups | — (node role's `AmazonEKS_CNI_Policy`) | `ENABLE_PREFIX_DELEGATION=true`, `WARM_PREFIX_TARGET=1` |
| `kube-proxy` | before node groups | — | defaults |
| `eks-pod-identity-agent` | after node groups | — | defaults |
| `coredns` | after node groups | — | defaults |
| `aws-ebs-csi-driver` | after node groups | `kubespin-<id>-ebs-csi` (`AmazonEBSCSIDriverPolicy`) | defaults |
| `aws-efs-csi-driver` | after node groups; also on Auto Mode | `kubespin-<id>-efs-csi` (`AmazonEFSCSIDriverPolicy`) | defaults |
| `cert-manager` | after node groups | — | defaults |
| `external-dns` | after node groups | `kubespin-<id>-external-dns` (inline: record-set writes, zone reads) | `txtOwnerId: <cluster id>` |
| `fluent-bit` | after node groups | — | defaults |

- **Prefix delegation:** each ENI slot provides a /28 prefix instead of a single IP. That raises a node's pod limit from its ENI-bound value (17 on a `t3.medium`) to EKS's cap of 110. `vpc-cni` is installed before any node group because a managed node group sets its nodes' max-pods from the CNI configuration when the group is created. Prefix delegation needs Nitro instances, so `instanceType: auto` only selects Nitro types.
- **Why `vpc-cni` keeps the node role:** it must hand out pod IPs before the Pod Identity agent can start, so it doesn't use Pod Identity.
- **Pod Identity is the default for every add-on that needs AWS permissions.** `ensureManagedAddon` creates a role trusting only `pods.eks.amazonaws.com` and passes it on the add-on itself (`CreateAddon`/`UpdateAddon` `PodIdentityAssociations`). No IAM OIDC provider is registered.
- **IRSA-era add-ons are left alone:** an add-on an older kubespin bound through IRSA (a `ServiceAccountRoleArn`) keeps that binding, because it still works. Moving it to Pod Identity is a deliberate, separate step, not something every apply retries.
- **Adopting existing installs:** `ResolveConflicts=OVERWRITE` adopts what's already running: the unmanaged `vpc-cni`/`kube-proxy`/`coredns` EKS installs on every cluster, and Helm releases an older kubespin delivered through Argo CD.
- **No-op applies stay no-op:** configuration drift is compared as JSON, so key order and whitespace don't count. An unchanged add-on costs one `DescribeAddon`, plus role reads when it has an identity, and no writes.
- **Auto Mode:** it runs its own networking, DNS, block storage and Pod Identity agent, so only `aws-efs-csi-driver` is installed there.
- **Kept as Helm charts on AWS:** there's no EKS add-on running the same software for `cluster-autoscaler`, kube-prometheus-stack, kyverno, ingress-nginx, external-secrets, velero, falco, the OTel collector, or gateway-api. `adot` sends to CloudWatch/X-Ray rather than acting as a generic collector, and `kubecost` is a Marketplace product.

## autoscaler.go

`ensureClusterAutoscalerIdentity` gives the catalog's `cluster-autoscaler` addon its AWS permissions. It runs on every non-Auto-Mode `Create`/`Reconcile`, and each step is a no-op when nothing has drifted:

1. Ensures role `kubespin-<id>-cluster-autoscaler`, trusting `pods.eks.amazonaws.com`, with an inline policy. The policy allows the read-only Auto Scaling/EC2 discovery calls everywhere, but `SetDesiredCapacity`/`TerminateInstanceInAutoScalingGroup` only on groups tagged `k8s.io/cluster-autoscaler/<cluster>=owned`.
2. Binds that role to `kube-system/cluster-autoscaler` with an EKS Pod Identity association.

AWS ships no EKS add-on for cluster-autoscaler, so it stays a catalog Helm chart. Its association is created directly (`CreatePodIdentityAssociation`) rather than through `CreateAddon`, and the chart's values only need to name the service account. It depends on the `eks-pod-identity-agent` add-on from `addons.go`. `Delete` removes the role; `deleteRole` now also deletes inline policies, because IAM refuses to delete a role that still has one.

## aws.go

### `eksAPI`, `iamAPI`, `ec2API` interfaces

Narrow interfaces over the AWS SDK v2 clients.

<details>
<summary>Signature</summary>

- **`eksAPI`:** `DescribeCluster`, `CreateCluster`, `UpdateClusterConfig`, `DeleteCluster`, `ListNodegroups`, `DescribeNodegroup`, `CreateNodegroup`, `UpdateNodegroupConfig`, `DeleteNodegroup`, `DescribeAddon`, `CreateAddon`, `UpdateAddon`, `ListPodIdentityAssociations`, `CreatePodIdentityAssociation`.
- **`iamAPI`:** service roles and add-on Pod Identity roles — `GetRole`, `CreateRole`, `DeleteRole`, `UpdateAssumeRolePolicy`, `AttachRolePolicy`, `ListAttachedRolePolicies`, `DetachRolePolicy`, `GetRolePolicy`, `PutRolePolicy`, `ListRolePolicies`, `DeleteRolePolicy`, `ListInstanceProfilesForRole`, `RemoveRoleFromInstanceProfile`; plus `ListOpenIDConnectProviders`, `GetOpenIDConnectProvider`, `DeleteOpenIDConnectProvider`, used only to remove the OIDC provider an IRSA-era cluster had.
- **`ec2API`:** when `spec.Subnets` is empty, VPC/subnet/IGW/route-table creation (`DescribeVpcs`, `CreateVpc`, `ModifyVpcAttribute`, `DescribeAvailabilityZones`, `DescribeSubnets`, `CreateSubnet`, `DescribeInternetGateways`, `CreateInternetGateway`, `AttachInternetGateway`, `DescribeRouteTables`, `CreateRouteTable`, `CreateRoute`, `AssociateRouteTable`); for a node pool with `instanceType: auto`, the spot price lookup at node-group creation (`DescribeSubnets`, `DescribeInstanceTypes`, `DescribeInstanceTypeOfferings`, `DescribeSpotPriceHistory`).

</details>

### `Clients`

<details>
<summary>Signature</summary>

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

- **Behavior:**
    - `NewClients` loads the default AWS config for `region` (`config.LoadDefaultConfig`) and builds real `eks`, `iam`, `ec2` clients plus an STS presign client.
    - Every provisioner type below (`ClusterProvisioner`, `NetworkProvisioner`) wraps a shared `*Clients`.

</details>

### AWS-managed policy / OIDC constants

<details>
<summary>Signature</summary>

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

- **Invariant:** AWS-managed policies are attached rather than authored, so the cluster stays current as AWS extends what EKS control planes and nodes need.

</details>

### `names` — deterministic resource naming

<details>
<summary>Signature</summary>

```go
type names struct{ spec core.ClusterSpec }

func (n names) cluster() string
func (n names) clusterRole() string             // "kubespin-<clusterID>-cluster"
func (n names) nodeRole() string                // "kubespin-<clusterID>-node"
func (n names) nodeGroup(pool string) string     // "<clusterID>-<pool>"
func (n names) addonRole(comp string) string     // "kubespin-<clusterID>-<comp>" (Pod Identity role)
func (n names) vpcName() string                  // "kubespin-<clusterID>"
func (n names) subnetName(az string) string      // "kubespin-<clusterID>-subnet-<az>"
func (n names) igwName() string                  // "kubespin-<clusterID>-igw"
func (n names) routeTableName() string           // "kubespin-<clusterID>-rt"
```

```go
func tags(spec core.ClusterSpec) map[string]string
```

- **Behavior:** `tags` returns the common tag set applied to every AWS resource: `ManagedBy: kubespin`, `kubespin/cluster: <ID>`, `kubespin/profile: <profile>`.
- **Invariant:** every AWS resource kubespin creates is name-derived from the cluster ID, so resources are identifiable and a second cluster cannot collide with them; this same deterministic naming is what lets `EnsureNetwork` and `ensureRole` adopt existing resources on a resumed `apply` instead of duplicating them.

</details>

## cluster.go

EKS cluster and node group lifecycle.

### `ClusterProvisioner`

Implements `provisioner.ClusterProvisioner` and (via `kubeauth.go`) `provisioner.RESTConfigProvisioner`.

<details>
<summary>Signature</summary>

```go
type ClusterProvisioner struct {
    c    *Clients
    wait provisioner.WaitOptions // tunes the polls Delete makes while node groups drain
}

func NewClusterProvisioner(c *Clients) *ClusterProvisioner
func (p *ClusterProvisioner) Provider() core.Provider // core.ProviderAWS
```

</details>

<details>
<summary>`Create(ctx, spec) error`</summary>

- **Behavior:**
    - Validates the spec (`validateForEKS`).
    - Ensures the EKS cluster service role exists with `AmazonEKSClusterPolicy` attached.
    - `Describe`s the cluster. If absent, calls `createCluster` and returns — node groups cannot attach until the control plane is active, so they are deferred to `Reconcile` once the caller has polled to active.
    - If already active, calls `ensureComputeAndAddons` directly (covers a resumed run that crashed after cluster creation but before node groups/addons).
- `createCluster` issues `eks.CreateCluster` with:

```go
&eks.CreateClusterInput{
    Name:               names{spec}.cluster(),
    RoleArn:            clusterRoleARN,
    ResourcesVpcConfig: vpcConfig(spec),
    Tags:               tags(spec),
    Version:            spec.KubernetesVersion, // only if non-empty
}
```

- **Invariant:** `ekstypes.ResourceInUseException` from `CreateCluster` is treated as convergence (another run got there first), not failure.

</details>

<details>
<summary>`vpcConfig(spec) *ekstypes.VpcConfigRequest`</summary>

Translates access mode into EKS endpoint config:

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

- **Behavior:**
    - A private cluster (`Access: private`) has no public endpoint at all.
    - A public cluster is reachable but restricted to `AuthorizedCIDRs` when any are given.
    - Both keep the private endpoint enabled so in-VPC traffic never leaves the network.
- **Invariant:** this is the same function `reconcileAccess` calls to flip access mode later, so creation and reconciliation share one source of truth for endpoint config.

</details>

#### `ClusterState`

<details>
<summary>`Describe(ctx, spec) (provisioner.ClusterState, error)`</summary>

- **Behavior:**
    - Calls `eks.DescribeCluster`; a `ekstypes.ResourceNotFoundException` maps to `StatusAbsent` with no error.
    - Otherwise builds `ClusterState`: `Status` via `normaliseStatus`, `Endpoint`, `Version`, `Access` via `accessFrom(cluster.ResourcesVpcConfig)`, `OIDCIssuer` from `cluster.Identity.Oidc.Issuer`, `NetworkID` from `cluster.ResourcesVpcConfig.ClusterSecurityGroupId`, and `CertificateAuthorityData` base64-decoded from `cluster.CertificateAuthority.Data`.
    - When the cluster is active, also populates `NodePools` via `describeNodePools`.
- `normaliseStatus` maps EKS's `ClusterStatus` to the shared `provisioner.Status`:
    - `Active→Active`, `Creating→Creating`, `Updating→Updating`, `Deleting→Deleting`
    - `Pending→Creating` (deliberately grouped with waiting, not failure — EKS reports Pending for a cluster that has not started yet and it clears on its own)
    - `Failed`/anything else `→Failed`
- `accessFrom(cfg)` returns `core.AccessPublic` if `cfg.EndpointPublicAccess` is true, else `core.AccessPrivate`.
- `describeNodePools` lists node groups (`ListNodegroups`) then `DescribeNodegroup`s each, mapping to `core.NodePool{Name, Labels, DiskSizeGB, InstanceType (first entry), MinSize, MaxSize, DesiredSize}`. `poolNameFromNodeGroup` strips the `<clusterID>-` prefix EKS's node group name carries to recover the pool name from `spec.NodePools`. Results are sorted by name for deterministic output.

</details>

#### `Change`

<details>
<summary>`Reconcile(ctx, spec) (provisioner.Change, error)`</summary>

- **Behavior:** `Describe`s the cluster (errors if `StatusAbsent`, wrapping `provisioner.ErrNotFound`), then merges the `Change` from `reconcileAccess` and `ensureComputeAndAddons`. That runs, in order: the network add-ons, `ensureNodeGroups`, the workload add-ons, and `ensureClusterAutoscalerIdentity`. Auto Mode skips the node groups and the autoscaler identity, and gets only the add-ons it lacks.
- `reconcileAccess`: compares `state.Access` to `spec.Access`; if they differ, calls `eks.UpdateClusterConfig` with the same `vpcConfig(spec)` used at creation, and reports a `Change` detail `"access <old> -> <new>"`.
- `ensureNodeGroups`:
    - First ensures the node IAM role (`AmazonEKSWorkerNodePolicy`, `AmazonEKS_CNI_Policy`, `AmazonEC2ContainerRegistryReadOnly`) exists.
    - For each pool in `spec.NodePools`: if missing, calls `createNodeGroup`; if present and sizing (`MinSize`/`MaxSize`/`DesiredSize`) differs, calls `eks.UpdateNodegroupConfig` to resize.
    - **Never deletes a node group** — removing a pool would evict running workloads, a decision reserved for a human.
    - Each create/resize is recorded into the `*provisioner.Change` via the `record` helper.
- `createNodeGroup` calls `eks.CreateNodegroup` with subnets from `spec.Subnets`, the pool's single `InstanceType`, scaling config, labels, and tags; `ekstypes.ResourceInUseException` is treated as convergence.

</details>

<details>
<summary>`Delete(ctx, spec) error`</summary>

- **Behavior:** tears down everything `Create` provisioned, not just the cluster resource itself — EKS's own `DeleteCluster` removes only the cluster, so nothing else here is cleaned up on its own:
    - `Describe`s first. If the cluster is not already `StatusAbsent`/`StatusDeleting`: lists node groups and deletes each (`DeleteNodegroup`) — node groups must go first because EKS refuses to delete a cluster with any attached; calls `waitForNodeGroupsGone` if any existed; deletes the `nodeRole` (nodes are fully terminated by this point, so its instance-profile job is done); requests `eks.DeleteCluster`.
    - Always, whichever branch above ran: deletes the `clusterRole`, the Auto Mode node role, every add-on Pod Identity role (`identityRoles`), and the cluster-autoscaler role. EKS removes Pod Identity associations along with the cluster. Then, if `Describe` reported an `OIDCIssuer`, it calls `deleteOIDCProvider`: a cluster created by an IRSA-era kubespin has an IAM OIDC provider, found by issuer host; a newer cluster has none, so this does nothing.
    - `NoSuchEntityException`/`ResourceNotFoundException` at any step converges rather than erroring, so a retried teardown resumes cleanly — including a retry against a cluster an earlier, interrupted run already left `StatusDeleting`, which still reaches the role/OIDC cleanup rather than short-circuiting past it.
    - **Known gap:** if a cluster finishes deleting entirely between one `Delete` call and the next, `Describe` can no longer report its OIDC issuer (EKS drops it once the cluster is gone), so a delete resumed only after that point cannot find the OIDC provider by issuer host and leaves it behind. Narrow — deletion takes minutes — but real.
- `waitForNodeGroupsGone` polls `ListNodegroups` on `p.wait.Interval`/`p.wait.Timeout` (falling back to `provisioner.DefaultWaitOptions()` values if unset) until the list is empty, because `DeleteNodegroup` only accepts the request — draining and terminating nodes takes minutes, and `DeleteCluster` fails with `ResourceInUseException` the whole time.
- `deleteRole(ctx, name)` — detaches every attached policy, deletes every inline policy, and removes the role from any instance profile (IAM refuses to delete a role with any of these left), then `DeleteRole`; `NoSuchEntityException` converges.
- `deleteOIDCProvider(ctx, issuer)` — `ListOpenIDConnectProviders`, `GetOpenIDConnectProvider`s each to compare its `Url` against the issuer host, and `DeleteOpenIDConnectProvider`s the match; no match found is a no-op, not an error.

</details>

### EKS add-ons

See [addons.go](#addonsgo). `ensureManagedAddon` replaces the CSI-only `ensureCSIAddons`/`ensureAddon` of the IRSA era.

### Role helpers

<details>
<summary>Signature</summary>

```go
func ensureRole(ctx context.Context, name string, trust map[string]any, policies []string) (string, error)
func attachPolicies(ctx context.Context, role string, policies []string) error
func eksServiceTrust(service string) map[string]any
```

- **Behavior:**
    - `ensureRole` — `GetRole`; if `NoSuchEntityException`, marshals `trust` to JSON and `CreateRole`s it. Either way, calls `attachPolicies` to reconcile the policy set, and returns the role ARN.
    - `attachPolicies` — lists currently attached policies and attaches any from `policies` not already present (never detaches).
    - `eksServiceTrust` — builds a trust policy allowing `sts:AssumeRole` for the given AWS service principal (`eks.amazonaws.com` for the cluster role, `ec2.amazonaws.com` for the node role).
    - `deleteRole`/`deleteOIDCProvider` (Delete's counterparts) are documented above, under `Delete`.

</details>

### Validation and misc

<details>
<summary>Signature</summary>

```go
func validateForEKS(spec core.ClusterSpec) error
func findPool(pools []core.NodePool, name string) (core.NodePool, bool)
func record(change *provisioner.Change, detail string) // no-op if change is nil (Create passes nil — creation isn't a reconcile finding)
```

- **Behavior:** `validateForEKS` rejects specs with fewer than 2 subnets — EKS places the control plane's cross-account ENIs in at least two Availability Zones and rejects fewer at creation time. Wraps `core.ErrInvalidSpec`.

</details>

## kubeauth.go — REST config / bearer token minting

<details>
<summary>Signature</summary>

```go
const eksTokenPrefix = "k8s-aws-v1."

type stsPresignAPI interface {
    PresignGetCallerIdentityURL(ctx context.Context, clusterName string) (string, error)
}

type stsPresigner struct{ client *sts.PresignClient }

func newSTSPresigner(cfg aws.Config) *stsPresigner
func (p *stsPresigner) PresignGetCallerIdentityURL(ctx context.Context, clusterName string) (string, error)
```

- **Behavior:** `PresignGetCallerIdentityURL` presigns an STS `GetCallerIdentity` request and injects an `x-k8s-aws-id: <clusterName>` header via a Smithy build middleware — this is what `aws-iam-authenticator` (built into every EKS control plane) checks to scope the token to one cluster; a token presigned for a different cluster name is rejected.

</details>

<details>
<summary>`RESTConfig(ctx, spec) (*rest.Config, error)`</summary>

Satisfies `provisioner.RESTConfigProvisioner`.

```go
func (p *ClusterProvisioner) RESTConfig(ctx context.Context, spec core.ClusterSpec) (*rest.Config, error)
```

- **Behavior:**
    - `Describe`s the cluster (must be `StatusActive`).
    - Mints a presigned URL via `p.c.sts.PresignGetCallerIdentityURL`.
    - Returns:

```go
&rest.Config{
    Host:            state.Endpoint,
    BearerToken:     "k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte(url)),
    TLSClientConfig: rest.TLSClientConfig{CAData: state.CertificateAuthorityData},
}
```

- **Invariant:** no static credential is ever written down — the token is derived fresh from whatever session `kubespin login` cached, matching the format `aws eks get-token` produces, and its lifetime is bounded by the presigned URL's default 60-second `X-Amz-Expires`.

</details>

## network.go

VPC/subnet auto-creation and teardown.

<details>
<summary>Constants</summary>

```go
const (
    defaultVPCCIDR  = "10.0.0.0/16"
    subnetPrefixLen = 24 // each carved subnet
    subnetsWanted   = 2  // EKS control plane minimum
)
```

</details>

### `NetworkProvisioner`

<details>
<summary>Signature</summary>

```go
type NetworkProvisioner struct {
    c *Clients
}

func NewNetworkProvisioner(c *Clients) *NetworkProvisioner
func (p *NetworkProvisioner) Provider() core.Provider // core.ProviderAWS
```

</details>

<details>
<summary>`EnsureNetwork(ctx, spec) (provisioner.NetworkResult, error)`</summary>

- **Behavior:**
    - If `spec.Subnets` is already set, returns it unchanged with no `Change`.
    - Otherwise, using `vpcCIDR` (`spec.VPCCIDR` or `defaultVPCCIDR`):
        1. `ensureVPC` — looks up a VPC tagged `Name: kubespin-<clusterID>`; creates one if absent (`CreateVpc`), then enables DNS support and DNS hostnames via two `ModifyVpcAttribute` calls (EKS requires both; neither defaults on for a new VPC).
        2. `availabilityZones` — lists available AZs in the region (`DescribeAvailabilityZones`, filtered to `state=available`), sorted alphabetically so the pair chosen is deterministic across runs. Errors if the region has fewer than `subnetsWanted` (2) AZs.
        3. For `i in [0, 1]`: `carveSubnetCIDR(vpcCIDR, i)` computes the i-th `/24` block of the VPC CIDR, then `ensureSubnet` looks up (by `Name: kubespin-<clusterID>-subnet-<az>` tag) or creates (`CreateSubnet`) a subnet in `azs[i]` with that CIDR.
        4. `ensureInternetGateway` — looks up or creates (`CreateInternetGateway` + `AttachInternetGateway`) an IGW tagged `kubespin-<clusterID>-igw`.
        5. `ensureRouteTable` — looks up a route table tagged `kubespin-<clusterID>-rt`; if none exists, creates one, adds a default route (`0.0.0.0/0` via the IGW), and associates both subnets with it.
    - Returns `NetworkResult{SubnetIDs: [2 subnet IDs], Change}`.
- **Invariant:**
    - **A single shared public route table is used for both subnets** — kubespin does not split into private subnets behind a NAT gateway; that is out of scope (expensive to run/test, and the architecture only requires nothing reach *in*, not that nodes be unreachable from their own VPC's egress path).
    - Every step is create-or-adopt, keyed off the deterministic `Name` tag from `names{spec}`, so a resumed or repeated `apply` converges onto the same VPC/subnets/IGW/route table rather than duplicating them.

</details>

<details>
<summary>`carveSubnetCIDR(vpcCIDR, index) (string, error)`</summary>

- **Behavior:** parses `vpcCIDR` as IPv4, requires it to be at least a `/24` (errors otherwise), and computes the `index`-th `/24` block by adding `index * 256` to the base IP as a big-endian uint32.

</details>

#### `names`

<details>
<summary>`tagNameFilter(name)` / `tagSpec(resourceType, name, spec)`</summary>

- **Behavior:** build the EC2 `Name`-tag filter used for lookups, and the tag specification (`Name` + the common `tags(spec)` set) applied on creation, respectively.

</details>

## Access-mode summary (AWS)

| Access mode | `EndpointPrivateAccess` | `EndpointPublicAccess` | `PublicAccessCidrs` |
|---|---|---|---|
| `private` | `true` | `false` | not set |
| `public` | `true` | `true` | `spec.AuthorizedCIDRs` if non-empty, else unrestricted |

Both modes leave the private endpoint enabled so in-VPC traffic never has to leave the network. `vpcConfig` (`cluster.go`) is the single function that encodes this branching, used both at cluster creation and by `reconcileAccess` when `apply` detects `spec.Access` has changed.
