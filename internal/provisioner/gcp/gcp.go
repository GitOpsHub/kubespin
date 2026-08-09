// Package gcp provisions GKE clusters and Workload Identity bindings.
//
// Every GCP service is reached through an interface listing only the calls
// this package makes, the same discipline internal/provisioner/aws follows.
// That keeps the whole provisioner testable without credentials, and doubles
// as the precise permission set an operator has to grant.
package gcp

import (
	"context"
	"fmt"
	"log/slog"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	gax "github.com/googleapis/gax-go/v2"
	compute "google.golang.org/api/compute/v1"
	iam "google.golang.org/api/iam/v1"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// clusterAPI is the GKE Cluster Manager surface this package uses.
type clusterAPI interface {
	GetCluster(context.Context, *containerpb.GetClusterRequest, ...gax.CallOption) (*containerpb.Cluster, error)
	CreateCluster(context.Context, *containerpb.CreateClusterRequest, ...gax.CallOption) (*containerpb.Operation, error)
	UpdateCluster(context.Context, *containerpb.UpdateClusterRequest, ...gax.CallOption) (*containerpb.Operation, error)
	DeleteCluster(context.Context, *containerpb.DeleteClusterRequest, ...gax.CallOption) (*containerpb.Operation, error)
	ListNodePools(context.Context, *containerpb.ListNodePoolsRequest, ...gax.CallOption) (*containerpb.ListNodePoolsResponse, error)
	GetNodePool(context.Context, *containerpb.GetNodePoolRequest, ...gax.CallOption) (*containerpb.NodePool, error)
	CreateNodePool(context.Context, *containerpb.CreateNodePoolRequest, ...gax.CallOption) (*containerpb.Operation, error)
	SetNodePoolSize(context.Context, *containerpb.SetNodePoolSizeRequest, ...gax.CallOption) (*containerpb.Operation, error)
	DeleteNodePool(context.Context, *containerpb.DeleteNodePoolRequest, ...gax.CallOption) (*containerpb.Operation, error)
}

// serviceAccountsAPI covers the IAM service accounts Workload Identity binds to.
type serviceAccountsAPI interface {
	Get(ctx context.Context, name string) (*iam.ServiceAccount, error)
	Create(ctx context.Context, parent string, req *iam.CreateServiceAccountRequest) (*iam.ServiceAccount, error)
	Delete(ctx context.Context, name string) error
	GetIamPolicy(ctx context.Context, resource string) (*iam.Policy, error)
	SetIamPolicy(ctx context.Context, resource string, req *iam.SetIamPolicyRequest) (*iam.Policy, error)
}

// firewallsAPI is used only for the status reporter's egress rule.
//
// Its Get method is named GetFirewall rather than Get: a single fake stands in
// for both this and serviceAccountsAPI in tests, and Go does not allow a type
// to implement two same-named methods with different signatures.
type firewallsAPI interface {
	GetFirewall(ctx context.Context, project, name string) (*compute.Firewall, error)
	Insert(ctx context.Context, project string, fw *compute.Firewall) error
}

// networksAPI is used only by EnsureNetwork, when spec.Subnets is empty.
type networksAPI interface {
	GetNetwork(ctx context.Context, project, name string) (*compute.Network, error)
	InsertNetwork(ctx context.Context, project string, network *compute.Network) error
}

// subnetworksAPI is used only by EnsureNetwork, when spec.Subnets is empty.
type subnetworksAPI interface {
	GetSubnetwork(ctx context.Context, project, region, name string) (*compute.Subnetwork, error)
	InsertSubnetwork(ctx context.Context, project, region string, subnet *compute.Subnetwork) error
}

// Clients bundles the GCP clients the provisioner uses, scoped to one project.
//
// The project is fixed at construction, the way AWS's Clients fixes a region:
// a cluster's spec carries its location (zone or region) but not the project
// that owns it, which is operator configuration rather than cluster desired
// state.
type Clients struct {
	project     string
	cluster     clusterAPI
	svcAccts    serviceAccountsAPI
	firewalls   firewallsAPI
	networks    networksAPI
	subnetworks subnetworksAPI
	tokens      tokenAPI

	logger *slog.Logger
}

// Option configures Clients.
type Option func(*Clients)

// WithLogger sets the logger every provisioner built over these Clients logs
// through. Defaults to slog.Default() when not given.
func WithLogger(logger *slog.Logger) Option {
	return func(c *Clients) { c.logger = logger }
}

// NewClients builds real GCP clients for a project.
func NewClients(ctx context.Context, project string, opts ...Option) (*Clients, error) {
	if project == "" {
		return nil, fmt.Errorf("gcp: project is required")
	}

	cm, err := container.NewClusterManagerClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("building GKE client: %w", err)
	}

	iamSvc, err := iam.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("building IAM client: %w", err)
	}

	computeSvc, err := compute.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("building Compute client: %w", err)
	}

	c := &Clients{
		project:     project,
		cluster:     cm,
		svcAccts:    realServiceAccounts{iamSvc.Projects.ServiceAccounts},
		firewalls:   realFirewalls{computeSvc.Firewalls},
		networks:    realNetworks{computeSvc.Networks},
		subnetworks: realSubnetworks{computeSvc.Subnetworks},
		tokens:      applicationDefaultTokens{},
		logger:      slog.Default(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// realServiceAccounts adapts the fluent iam/v1 client to serviceAccountsAPI.
type realServiceAccounts struct {
	svc *iam.ProjectsServiceAccountsService
}

func (r realServiceAccounts) Get(ctx context.Context, name string) (*iam.ServiceAccount, error) {
	sa, err := r.svc.Get(name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("iam: get service account %s: %w", name, err)
	}
	return sa, nil
}

func (r realServiceAccounts) Create(
	ctx context.Context, parent string, req *iam.CreateServiceAccountRequest,
) (*iam.ServiceAccount, error) {
	sa, err := r.svc.Create(parent, req).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("iam: create service account %s/%s: %w", parent, req.AccountId, err)
	}
	return sa, nil
}

func (r realServiceAccounts) Delete(ctx context.Context, name string) error {
	if _, err := r.svc.Delete(name).Context(ctx).Do(); err != nil {
		return fmt.Errorf("iam: delete service account %s: %w", name, err)
	}
	return nil
}

func (r realServiceAccounts) GetIamPolicy(ctx context.Context, resource string) (*iam.Policy, error) {
	p, err := r.svc.GetIamPolicy(resource).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("iam: get IAM policy for %s: %w", resource, err)
	}
	return p, nil
}

func (r realServiceAccounts) SetIamPolicy(
	ctx context.Context, resource string, req *iam.SetIamPolicyRequest,
) (*iam.Policy, error) {
	p, err := r.svc.SetIamPolicy(resource, req).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("iam: set IAM policy for %s: %w", resource, err)
	}
	return p, nil
}

// realFirewalls adapts the fluent compute/v1 client to firewallsAPI.
type realFirewalls struct {
	svc *compute.FirewallsService
}

func (r realFirewalls) GetFirewall(ctx context.Context, project, name string) (*compute.Firewall, error) {
	fw, err := r.svc.Get(project, name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("compute: get firewall %s: %w", name, err)
	}
	return fw, nil
}

func (r realFirewalls) Insert(ctx context.Context, project string, fw *compute.Firewall) error {
	if _, err := r.svc.Insert(project, fw).Context(ctx).Do(); err != nil {
		return fmt.Errorf("compute: insert firewall %s: %w", fw.Name, err)
	}
	return nil
}

// realNetworks adapts the fluent compute/v1 client to networksAPI.
type realNetworks struct {
	svc *compute.NetworksService
}

func (r realNetworks) GetNetwork(ctx context.Context, project, name string) (*compute.Network, error) {
	n, err := r.svc.Get(project, name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("compute: get network %s: %w", name, err)
	}
	return n, nil
}

func (r realNetworks) InsertNetwork(ctx context.Context, project string, network *compute.Network) error {
	if _, err := r.svc.Insert(project, network).Context(ctx).Do(); err != nil {
		return fmt.Errorf("compute: insert network %s: %w", network.Name, err)
	}
	return nil
}

// realSubnetworks adapts the fluent compute/v1 client to subnetworksAPI.
type realSubnetworks struct {
	svc *compute.SubnetworksService
}

func (r realSubnetworks) GetSubnetwork(ctx context.Context, project, region, name string) (*compute.Subnetwork, error) {
	s, err := r.svc.Get(project, region, name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("compute: get subnetwork %s: %w", name, err)
	}
	return s, nil
}

func (r realSubnetworks) InsertSubnetwork(ctx context.Context, project, region string, subnet *compute.Subnetwork) error {
	if _, err := r.svc.Insert(project, region, subnet).Context(ctx).Do(); err != nil {
		return fmt.Errorf("compute: insert subnetwork %s: %w", subnet.Name, err)
	}
	return nil
}

// names derives every GCP resource name from the cluster ID, so a cluster's
// resources are identifiable and a second cluster cannot collide with them.
type names struct {
	project string
	spec    core.ClusterSpec
}

func (n names) location() string { return n.spec.Region }

func (n names) parent() string {
	return fmt.Sprintf("projects/%s/locations/%s", n.project, n.location())
}

func (n names) cluster() string { return n.spec.ID.String() }

func (n names) clusterPath() string {
	return fmt.Sprintf("%s/clusters/%s", n.parent(), n.cluster())
}

func (n names) nodePool(pool string) string { return pool }

func (n names) nodePoolPath(pool string) string {
	return fmt.Sprintf("%s/nodePools/%s", n.clusterPath(), n.nodePool(pool))
}

func (n names) serviceAccountID(comp string) string {
	// Service account IDs must be 6-30 chars, lowercase alphanumeric or hyphen.
	// "kubespin-" plus a 21-char cluster/component pair keeps this within
	// bounds for the cluster IDs core.ClusterID.Validate allows.
	id := "ksp-" + n.spec.ID.String() + "-" + comp
	if len(id) > 30 {
		id = id[:30]
	}
	return id
}

func (n names) serviceAccountEmail(comp string) string {
	return fmt.Sprintf("%s@%s.iam.gserviceaccount.com", n.serviceAccountID(comp), n.project)
}

func (n names) serviceAccountResource(comp string) string {
	return fmt.Sprintf("projects/%s/serviceAccounts/%s", n.project, n.serviceAccountEmail(comp))
}

func (n names) network() string { return "kubespin-" + n.spec.ID.String() }

func (n names) networkResource() string {
	return fmt.Sprintf("projects/%s/global/networks/%s", n.project, n.network())
}

func (n names) subnetwork() string { return "kubespin-" + n.spec.ID.String() + "-subnet" }

func (n names) subnetworkResource() string {
	return fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", n.project, n.location(), n.subnetwork())
}

func labels(spec core.ClusterSpec) map[string]string {
	return map[string]string{
		"managed-by":       "kubespin",
		"kubespin-cluster": spec.ID.String(),
		"kubespin-profile": sanitizeLabelValue(spec.Profile.String()),
	}
}

// sanitizeLabelValue keeps a profile ref ("tier-small@1.0.0") within GCP
// label value rules: lowercase letters, digits, hyphens, underscores.
func sanitizeLabelValue(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
