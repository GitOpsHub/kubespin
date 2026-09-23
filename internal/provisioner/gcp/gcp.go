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
	"slices"
	"strings"
	"time"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	gax "github.com/googleapis/gax-go/v2"
	compute "google.golang.org/api/compute/v1"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// clusterAPI is the GKE Cluster Manager surface this package uses.
type clusterAPI interface {
	GetCluster(context.Context, *containerpb.GetClusterRequest, ...gax.CallOption) (*containerpb.Cluster, error)
	// ListClusters is used only by locate, to find a cluster by name across
	// every location in the project when it is not at the path the spec
	// implies — see locate
	// for why that lookup exists at all.
	ListClusters(context.Context, *containerpb.ListClustersRequest, ...gax.CallOption) (*containerpb.ListClustersResponse, error)
	CreateCluster(context.Context, *containerpb.CreateClusterRequest, ...gax.CallOption) (*containerpb.Operation, error)
	UpdateCluster(context.Context, *containerpb.UpdateClusterRequest, ...gax.CallOption) (*containerpb.Operation, error)
	DeleteCluster(context.Context, *containerpb.DeleteClusterRequest, ...gax.CallOption) (*containerpb.Operation, error)
	ListNodePools(context.Context, *containerpb.ListNodePoolsRequest, ...gax.CallOption) (*containerpb.ListNodePoolsResponse, error)
	GetNodePool(context.Context, *containerpb.GetNodePoolRequest, ...gax.CallOption) (*containerpb.NodePool, error)
	CreateNodePool(context.Context, *containerpb.CreateNodePoolRequest, ...gax.CallOption) (*containerpb.Operation, error)
	// SetNodePoolAutoscaling converges a pool's min/max bounds. There is no
	// SetNodePoolSize: the pool's live node count belongs to the autoscaler,
	// and resetting it to the spec on every apply would fight it.
	SetNodePoolAutoscaling(context.Context, *containerpb.SetNodePoolAutoscalingRequest, ...gax.CallOption) (*containerpb.Operation, error)
	DeleteNodePool(context.Context, *containerpb.DeleteNodePoolRequest, ...gax.CallOption) (*containerpb.Operation, error)
}

// firewallsAPI exists only to clean up the egress firewall rule older
// kubespin versions created. Nothing creates one any more, but DeleteNetwork
// still removes a leftover: GCP refuses to delete a network that still has a
// firewall rule attached.
//
// Its Get method is named GetFirewall rather than Get: a single fake stands in
// for both this and the networks API in tests, and Go does not allow a type
// to implement two same-named methods with different signatures.
type firewallsAPI interface {
	GetFirewall(ctx context.Context, project, name string) (*compute.Firewall, error)
	DeleteFirewall(ctx context.Context, project, name string) error
}

// networksAPI is used only by EnsureNetwork, when spec.Subnets is empty.
type networksAPI interface {
	GetNetwork(ctx context.Context, project, name string) (*compute.Network, error)
	InsertNetwork(ctx context.Context, project string, network *compute.Network) error
	DeleteNetwork(ctx context.Context, project, name string) error
}

// subnetworksAPI is used only by EnsureNetwork, when spec.Subnets is empty.
type subnetworksAPI interface {
	GetSubnetwork(ctx context.Context, project, region, name string) (*compute.Subnetwork, error)
	InsertSubnetwork(ctx context.Context, project, region string, subnet *compute.Subnetwork) error
	DeleteSubnetwork(ctx context.Context, project, region, name string) error
}

// routersAPI is used only by EnsureNetwork, when spec.Subnets is empty.
//
// GKE cluster nodes are always created with EnablePrivateNodes (see
// privateClusterConfig), which leaves them with no public IP; a Cloud NAT
// behind a Cloud Router is the only way such a node reaches the public
// internet at all, including to pull addon images from a public registry.
// Without one, a kubespin-managed network builds a cluster whose nodes can
// never finish pulling any image.
type routersAPI interface {
	GetRouter(ctx context.Context, project, region, name string) (*compute.Router, error)
	InsertRouter(ctx context.Context, project, region string, router *compute.Router) error
	DeleteRouter(ctx context.Context, project, region, name string) error
}

// zonesAPI lists a region's zones, so a node pool is pinned to a zone that
// actually exists rather than an assumed "<region>-a" — us-east1 and
// europe-west1, for two, have no "-a" zone at all.
type zonesAPI interface {
	// ListZones returns the names of region's zones whose status is UP.
	ListZones(ctx context.Context, project, region string) ([]string, error)
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
	firewalls   firewallsAPI
	networks    networksAPI
	subnetworks subnetworksAPI
	routers     routersAPI
	zones       zonesAPI
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

	computeSvc, err := compute.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("building Compute client: %w", err)
	}

	c := &Clients{
		project:     project,
		cluster:     cm,
		firewalls:   realFirewalls{computeSvc.Firewalls, computeSvc.GlobalOperations},
		networks:    realNetworks{computeSvc.Networks, computeSvc.GlobalOperations},
		subnetworks: realSubnetworks{computeSvc.Subnetworks, computeSvc.RegionOperations},
		routers:     realRouters{computeSvc.Routers, computeSvc.RegionOperations},
		zones:       realZones{computeSvc.Zones},
		tokens:      applicationDefaultTokens{},
		logger:      slog.Default(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// DefaultZone returns the zone kubespin pins a region's node pools to when no
// zone is given: the first of the region's UP zones, in name order. It builds
// its own Compute client, for callers (the CLI's --spot handling) that need a
// zone before any provisioner exists.
func DefaultZone(ctx context.Context, project, region string) (string, error) {
	computeSvc, err := compute.NewService(ctx)
	if err != nil {
		return "", fmt.Errorf("building Compute client: %w", err)
	}
	return firstZone(ctx, realZones{computeSvc.Zones}, project, region)
}

// firstZone returns the first of region's UP zones, sorted by name so every
// run picks the same one.
func firstZone(ctx context.Context, zones zonesAPI, project, region string) (string, error) {
	listed, err := zones.ListZones(ctx, project, region)
	if err != nil {
		return "", fmt.Errorf("listing zones in %s: %w", region, err)
	}
	if len(listed) == 0 {
		return "", fmt.Errorf("%w: region %q has no available zones in project %s", core.ErrInvalidSpec, region, project)
	}
	return slices.Min(listed), nil
}

// realZones adapts the fluent compute/v1 client to zonesAPI.
type realZones struct {
	svc *compute.ZonesService
}

func (r realZones) ListZones(ctx context.Context, project, region string) ([]string, error) {
	var names []string
	suffix := "/regions/" + region
	err := r.svc.List(project).Pages(ctx, func(page *compute.ZoneList) error {
		for _, z := range page.Items {
			if z.Status == "UP" && strings.HasSuffix(z.Region, suffix) {
				names = append(names, z.Name)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("compute: list zones: %w", err)
	}
	return names, nil
}

// realFirewalls adapts the fluent compute/v1 client to firewallsAPI.
type realFirewalls struct {
	svc *compute.FirewallsService
	ops *compute.GlobalOperationsService
}

func (r realFirewalls) GetFirewall(ctx context.Context, project, name string) (*compute.Firewall, error) {
	fw, err := r.svc.Get(project, name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("compute: get firewall %s: %w", name, err)
	}
	return fw, nil
}

func (r realFirewalls) DeleteFirewall(ctx context.Context, project, name string) error {
	op, err := r.svc.Delete(project, name).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("compute: delete firewall %s: %w", name, err)
	}
	if err := waitGlobalOperation(ctx, r.ops, project, op); err != nil {
		return fmt.Errorf("compute: delete firewall %s: %w", name, err)
	}
	return nil
}

// realNetworks adapts the fluent compute/v1 client to networksAPI.
type realNetworks struct {
	svc *compute.NetworksService
	ops *compute.GlobalOperationsService
}

func (r realNetworks) GetNetwork(ctx context.Context, project, name string) (*compute.Network, error) {
	n, err := r.svc.Get(project, name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("compute: get network %s: %w", name, err)
	}
	return n, nil
}

func (r realNetworks) InsertNetwork(ctx context.Context, project string, network *compute.Network) error {
	op, err := r.svc.Insert(project, network).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("compute: insert network %s: %w", network.Name, err)
	}
	if err := waitGlobalOperation(ctx, r.ops, project, op); err != nil {
		return fmt.Errorf("compute: insert network %s: %w", network.Name, err)
	}
	return nil
}

func (r realNetworks) DeleteNetwork(ctx context.Context, project, name string) error {
	op, err := r.svc.Delete(project, name).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("compute: delete network %s: %w", name, err)
	}
	if err := waitGlobalOperation(ctx, r.ops, project, op); err != nil {
		return fmt.Errorf("compute: delete network %s: %w", name, err)
	}
	return nil
}

// realSubnetworks adapts the fluent compute/v1 client to subnetworksAPI.
type realSubnetworks struct {
	svc *compute.SubnetworksService
	ops *compute.RegionOperationsService
}

func (r realSubnetworks) GetSubnetwork(ctx context.Context, project, region, name string) (*compute.Subnetwork, error) {
	s, err := r.svc.Get(project, region, name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("compute: get subnetwork %s: %w", name, err)
	}
	return s, nil
}

func (r realSubnetworks) InsertSubnetwork(ctx context.Context, project, region string, subnet *compute.Subnetwork) error {
	op, err := r.svc.Insert(project, region, subnet).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("compute: insert subnetwork %s: %w", subnet.Name, err)
	}
	if err := waitRegionOperation(ctx, r.ops, project, region, op); err != nil {
		return fmt.Errorf("compute: insert subnetwork %s: %w", subnet.Name, err)
	}
	return nil
}

func (r realSubnetworks) DeleteSubnetwork(ctx context.Context, project, region, name string) error {
	op, err := r.svc.Delete(project, region, name).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("compute: delete subnetwork %s: %w", name, err)
	}
	if err := waitRegionOperation(ctx, r.ops, project, region, op); err != nil {
		return fmt.Errorf("compute: delete subnetwork %s: %w", name, err)
	}
	return nil
}

// realRouters adapts the fluent compute/v1 client to routersAPI.
type realRouters struct {
	svc *compute.RoutersService
	ops *compute.RegionOperationsService
}

func (r realRouters) GetRouter(ctx context.Context, project, region, name string) (*compute.Router, error) {
	rt, err := r.svc.Get(project, region, name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("compute: get router %s: %w", name, err)
	}
	return rt, nil
}

func (r realRouters) InsertRouter(ctx context.Context, project, region string, router *compute.Router) error {
	op, err := r.svc.Insert(project, region, router).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("compute: insert router %s: %w", router.Name, err)
	}
	if err := waitRegionOperation(ctx, r.ops, project, region, op); err != nil {
		return fmt.Errorf("compute: insert router %s: %w", router.Name, err)
	}
	return nil
}

func (r realRouters) DeleteRouter(ctx context.Context, project, region, name string) error {
	op, err := r.svc.Delete(project, region, name).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("compute: delete router %s: %w", name, err)
	}
	if err := waitRegionOperation(ctx, r.ops, project, region, op); err != nil {
		return fmt.Errorf("compute: delete router %s: %w", name, err)
	}
	return nil
}

// waitGlobalOperation and waitRegionOperation block until a Compute Engine
// v1 insert operation reaches DONE and surface its embedded async error, if
// any.
//
// Insert calls on this API are long-running operations: a successful Do()
// only means the request was accepted, not that the resource was actually
// created. A conflict discovered mid-operation (e.g. a CIDR range already in
// use elsewhere in the VPC) is reported by setting op.Error on the completed
// operation, not by an HTTP-level error, so callers that only check Do()'s
// return value observe a false success.
const operationPollInterval = 2 * time.Second

func waitGlobalOperation(ctx context.Context, ops *compute.GlobalOperationsService, project string, op *compute.Operation) error {
	for op.Status != "DONE" {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for operation %s: %w", op.Name, ctx.Err())
		case <-time.After(operationPollInterval):
		}
		var err error
		op, err = ops.Get(project, op.Name).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("polling operation %s: %w", op.Name, err)
		}
	}
	return operationError(op)
}

func waitRegionOperation(ctx context.Context, ops *compute.RegionOperationsService, project, region string, op *compute.Operation) error {
	for op.Status != "DONE" {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for operation %s: %w", op.Name, ctx.Err())
		case <-time.After(operationPollInterval):
		}
		var err error
		op, err = ops.Get(project, region, op.Name).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("polling operation %s: %w", op.Name, err)
		}
	}
	return operationError(op)
}

func operationError(op *compute.Operation) error {
	if op.Error == nil || len(op.Error.Errors) == 0 {
		return nil
	}
	msgs := make([]string, len(op.Error.Errors))
	for i, e := range op.Error.Errors {
		msgs[i] = e.Message
	}
	return fmt.Errorf("operation %s failed: %s", op.Name, strings.Join(msgs, "; "))
}

// names derives every GCP resource name from the cluster ID, so a cluster's
// resources are identifiable and a second cluster cannot collide with them.
type names struct {
	project string
	spec    core.ClusterSpec
	// located is the GKE location ClusterProvisioner.locate found the cluster
	// at — a zone or a region — overriding what spec implies.
	located string
}

// location is the region every regional resource (subnetwork, Cloud Router,
// Cloud NAT) is created in. It is always spec.Region, regardless of Zone —
// those resources have no zonal variant.
func (n names) location() string { return n.spec.Region }

// controlPlaneLocation is the GKE location segment used for the cluster
// itself: a zone when spec.Zone is set (a zonal, single-zone cluster,
// eligible for GCP's free-tier zonal cluster), otherwise the region (the
// default regional, multi-zone control plane).
func (n names) controlPlaneLocation() string {
	if n.located != "" {
		return n.located
	}
	if n.spec.Zone != "" {
		return n.spec.Zone
	}
	return n.spec.Region
}

func (n names) parent() string {
	return fmt.Sprintf("projects/%s/locations/%s", n.project, n.controlPlaneLocation())
}

func (n names) cluster() string { return n.spec.ID.String() }

func (n names) clusterPath() string {
	return fmt.Sprintf("%s/clusters/%s", n.parent(), n.cluster())
}

func (n names) nodePool(pool string) string { return pool }

func (n names) nodePoolPath(pool string) string {
	return fmt.Sprintf("%s/nodePools/%s", n.clusterPath(), n.nodePool(pool))
}

func (n names) network() string { return "kubespin-" + n.spec.ID.String() }

func (n names) networkResource() string {
	return fmt.Sprintf("projects/%s/global/networks/%s", n.project, n.network())
}

func (n names) subnetwork() string { return "kubespin-" + n.spec.ID.String() + "-subnet" }

func (n names) subnetworkResource() string {
	return fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", n.project, n.location(), n.subnetwork())
}

func (n names) router() string { return "kubespin-" + n.spec.ID.String() + "-router" }

func (n names) nat() string { return "kubespin-" + n.spec.ID.String() + "-nat" }

func labels(spec core.ClusterSpec) map[string]string {
	return map[string]string{
		"managed-by":       "kubespin",
		"kubespin-cluster": spec.ID.String(),
		"kubespin-size":    sanitizeLabelValue(spec.Size.String()),
	}
}

// sanitizeLabelValue keeps a value ("tier-small") within GCP label value
// rules: lowercase letters, digits, hyphens, underscores.
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
