// Package core holds the domain types shared by every other package.
//
// It is deliberately dependency-free: no cloud SDKs, no I/O, and no imports of
// other internal packages. Everything else in the tree imports core, so keeping
// it a leaf is what prevents import cycles as the tree grows.
package core

import (
	"errors"
	"fmt"
	"net"
	"regexp"
)

// ErrInvalidSpec is the sentinel wrapping every validation failure in this
// package. Callers branch with errors.Is rather than matching on message text.
var ErrInvalidSpec = errors.New("invalid spec")

// Provider is the cloud a cluster is provisioned on.
type Provider string

// Supported clouds. Each has an implementation under internal/provisioner.
const (
	ProviderAWS   Provider = "aws"
	ProviderGCP   Provider = "gcp"
	ProviderAzure Provider = "azure"
)

// Providers lists every supported cloud, in the order help text should show them.
func Providers() []Provider { return []Provider{ProviderAWS, ProviderGCP, ProviderAzure} }

// Valid reports whether p is a supported cloud.
func (p Provider) Valid() bool {
	switch p {
	case ProviderAWS, ProviderGCP, ProviderAzure:
		return true
	default:
		return false
	}
}

func (p Provider) String() string { return string(p) }

// Access is the cluster's API server exposure model.
//
// This is a first-class field rather than a per-cloud option because it branches
// behaviour in two distinct places: cluster creation (endpoint and authorized
// network configuration, per cloud) and addon templating (internal load balancer
// unless AccessPublic combines with an external ingress exposure).
type Access string

// Cluster API server exposure modes.
const (
	AccessPrivate Access = "private"
	AccessPublic  Access = "public"
)

// Valid reports whether a is a known access mode.
func (a Access) Valid() bool { return a == AccessPrivate || a == AccessPublic }

func (a Access) String() string { return string(a) }

// clusterIDPattern constrains ClusterID to what is simultaneously legal as a
// GitHub repository suffix, a DNS label, and a cloud resource name.
var clusterIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,38}[a-z0-9]$`)

// ClusterID uniquely identifies a cluster across the whole fleet. It is the
// Fleet Registry partition key and the suffix of the cluster's repository name,
// so it is immutable once a cluster reaches PhaseClusterCreated.
type ClusterID string

// Validate reports whether the ID is well formed.
func (id ClusterID) Validate() error {
	if id == "" {
		return fmt.Errorf("%w: cluster id is required", ErrInvalidSpec)
	}
	if !clusterIDPattern.MatchString(string(id)) {
		return fmt.Errorf(
			"%w: cluster id %q must be 3-40 chars, lowercase alphanumeric or hyphen, starting with a letter and ending alphanumeric",
			ErrInvalidSpec, string(id))
	}
	return nil
}

func (id ClusterID) String() string { return string(id) }

// CapacityType selects the purchasing option for a node pool's instances.
type CapacityType string

// Node pool capacity types. CapacityTypeOnDemand is also the zero value, so
// an unset CapacityType (every spec written before this field existed)
// behaves exactly as before.
const (
	CapacityTypeOnDemand CapacityType = "on-demand"
	CapacityTypeSpot     CapacityType = "spot"
)

// Valid reports whether c is empty (defaults to on-demand) or a known
// capacity type.
func (c CapacityType) Valid() bool {
	return c == "" || c == CapacityTypeOnDemand || c == CapacityTypeSpot
}

func (c CapacityType) String() string { return string(c) }

// NodePool is a homogeneous group of worker nodes. Sizing changes here are
// infra diffs: they resolve to a cloud SDK reconcile, never to a git commit.
type NodePool struct {
	Name         string            `yaml:"name" json:"name"`
	InstanceType string            `yaml:"instanceType" json:"instanceType"`
	MinSize      int32             `yaml:"minSize" json:"minSize"`
	MaxSize      int32             `yaml:"maxSize" json:"maxSize"`
	DesiredSize  int32             `yaml:"desiredSize" json:"desiredSize"`
	DiskSizeGB   int32             `yaml:"diskSizeGB,omitempty" json:"diskSizeGB,omitempty"`
	Labels       map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`

	// CapacityType requests spot/preemptible instances instead of on-demand.
	// Empty means on-demand. AWS and GCP honor this; AKS requires its
	// default/system node pool to stay on-demand, so it is a no-op there.
	CapacityType CapacityType `yaml:"capacityType,omitempty" json:"capacityType,omitempty"`
}

// Validate checks a single node pool in isolation. Cross-pool checks (unique
// names) live on ClusterSpec.
func (np NodePool) Validate() error {
	var errs []error
	if np.Name == "" {
		errs = append(errs, fmt.Errorf("%w: node pool name is required", ErrInvalidSpec))
	}
	if np.InstanceType == "" {
		errs = append(errs, fmt.Errorf("%w: node pool %q: instance type is required", ErrInvalidSpec, np.Name))
	}
	if !np.CapacityType.Valid() {
		errs = append(errs, fmt.Errorf("%w: node pool %q: capacityType %q must be on-demand or spot",
			ErrInvalidSpec, np.Name, np.CapacityType))
	}
	if np.MinSize < 0 {
		errs = append(errs, fmt.Errorf("%w: node pool %q: minSize must not be negative", ErrInvalidSpec, np.Name))
	}
	if np.MaxSize < 1 {
		errs = append(errs, fmt.Errorf("%w: node pool %q: maxSize must be at least 1", ErrInvalidSpec, np.Name))
	}
	if np.MinSize > np.MaxSize {
		errs = append(errs, fmt.Errorf("%w: node pool %q: minSize %d exceeds maxSize %d",
			ErrInvalidSpec, np.Name, np.MinSize, np.MaxSize))
	}
	if np.DesiredSize < np.MinSize || np.DesiredSize > np.MaxSize {
		errs = append(errs, fmt.Errorf("%w: node pool %q: desiredSize %d is outside [%d, %d]",
			ErrInvalidSpec, np.Name, np.DesiredSize, np.MinSize, np.MaxSize))
	}
	if np.DiskSizeGB < 0 {
		errs = append(errs, fmt.Errorf("%w: node pool %q: diskSizeGB must not be negative", ErrInvalidSpec, np.Name))
	}
	return errors.Join(errs...)
}

// ClusterSpec is the desired state of one cluster: the contents of the
// cluster.yaml in that cluster's repository.
type ClusterSpec struct {
	ID                ClusterID   `yaml:"id" json:"id"`
	Provider          Provider    `yaml:"provider" json:"provider"`
	Region            string      `yaml:"region" json:"region"`
	Access            Access      `yaml:"access" json:"access"`
	KubernetesVersion string      `yaml:"kubernetesVersion,omitempty" json:"kubernetesVersion,omitempty"`
	NodePools         []NodePool  `yaml:"nodePools" json:"nodePools"`
	Size              ClusterSize `yaml:"size" json:"size"`

	// Zone, when set, requests a zonal GKE cluster (control plane in a single
	// zone) instead of the default regional cluster. GCP-only; ignored on
	// AWS/Azure. A zonal cluster has no HA control plane but is what makes a
	// GCP cluster eligible for the one-free-zonal-cluster-per-billing-account
	// tier, so it exists for cost-sensitive dev clusters. Empty preserves the
	// existing regional behavior.
	Zone string `yaml:"zone,omitempty" json:"zone,omitempty"`

	// PublicNodes, when true, skips GKE's private-nodes configuration and the
	// Cloud Router/Cloud NAT it requires, giving nodes public IPs instead.
	// GCP-only; ignored on AWS/Azure. Trades network isolation for avoiding
	// Cloud NAT's always-on hourly + data processing charges — meant for
	// cost-sensitive dev clusters, not production.
	PublicNodes bool `yaml:"publicNodes,omitempty" json:"publicNodes,omitempty"`

	// AuthorizedCIDRs restricts API server access. It is meaningful only for
	// AccessPublic; a private cluster has no public endpoint to restrict.
	AuthorizedCIDRs []string `yaml:"authorizedCIDRs,omitempty" json:"authorizedCIDRs,omitempty"`

	// Subnets place the cluster on an existing network, named in whatever form
	// the provider uses: subnet IDs on AWS, a subnetwork on GCP, a subnet
	// resource ID on Azure.
	//
	// When Subnets is empty, EnsureNetwork creates a network for the cluster
	// on every provider rather than erroring — a VPC/subnets on AWS, a VPC
	// network/subnetwork on GCP, a VNet/subnet on Azure. Leaving Subnets empty
	// here — in the persisted cluster.yaml, not just the CLI flag — is what
	// durably means "kubespin manages this cluster's network." EnsureNetwork
	// derives deterministic names from the cluster ID and is create-or-adopt,
	// so every resumed or repeated apply converges to the same resources
	// without kubespin ever writing a discovered subnet ID back into the spec.
	// An operator who already owns a network's IP plan, peering, and egress
	// rules can still supply Subnets up front, and it is passed through
	// unchanged.
	Subnets []string `yaml:"subnets" json:"subnets"`

	// VPCCIDR sizes the VPC kubespin creates on AWS when Subnets is empty.
	// Meaningful only for ProviderAWS with Subnets unset; ignored otherwise.
	// Empty means "use kubespin's default."
	VPCCIDR string `yaml:"vpcCIDR,omitempty" json:"vpcCIDR,omitempty"`

	// VNetCIDR sizes the VNet kubespin creates on Azure when Subnets is
	// empty. Meaningful only for ProviderAzure with Subnets unset; ignored
	// otherwise. Empty means "use kubespin's default."
	VNetCIDR string `yaml:"vnetCIDR,omitempty" json:"vnetCIDR,omitempty"`

	// SubnetCIDR sizes the single subnet kubespin creates when Subnets is
	// empty, on either Azure (its cluster subnet) or GCP (its subnetwork).
	// Ignored on AWS, which carves two subnets out of VPCCIDR instead of
	// taking an explicit per-subnet size. Empty means "use kubespin's default."
	SubnetCIDR string `yaml:"subnetCIDR,omitempty" json:"subnetCIDR,omitempty"`

	// Overrides is this cluster's per-cluster patch onto Profile's resolved
	// addon set. It lives here, in the user-authored cluster.yaml, rather than
	// in a separate file: the addons.yaml the catalog resolves to is derived
	// state, not something a cluster owner edits directly.
	Overrides []AddonOverride `yaml:"overrides,omitempty" json:"overrides,omitempty"`
}

var kubernetesVersionPattern = regexp.MustCompile(`^\d+\.\d+$`)

// Validate returns every problem with the spec at once, joined, so a user fixing
// a spec sees the full list rather than one error per run.
func (s ClusterSpec) Validate() error {
	var errs []error

	if err := s.ID.Validate(); err != nil {
		errs = append(errs, err)
	}
	if !s.Provider.Valid() {
		errs = append(errs, fmt.Errorf("%w: provider %q must be one of aws, gcp, azure", ErrInvalidSpec, s.Provider))
	}
	if s.Region == "" {
		errs = append(errs, fmt.Errorf("%w: region is required", ErrInvalidSpec))
	}
	if !s.Access.Valid() {
		errs = append(errs, fmt.Errorf("%w: access %q must be private or public", ErrInvalidSpec, s.Access))
	}
	if s.KubernetesVersion != "" && !kubernetesVersionPattern.MatchString(s.KubernetesVersion) {
		errs = append(errs, fmt.Errorf("%w: kubernetesVersion %q must be MAJOR.MINOR", ErrInvalidSpec, s.KubernetesVersion))
	}
	if s.Access == AccessPrivate && len(s.AuthorizedCIDRs) > 0 {
		errs = append(errs, fmt.Errorf("%w: authorizedCIDRs is meaningless for a private cluster", ErrInvalidSpec))
	}
	if s.Provider != ProviderGCP && s.Zone != "" {
		errs = append(errs, fmt.Errorf("%w: zone is meaningful only for provider gcp", ErrInvalidSpec))
	}
	if s.Provider != ProviderGCP && s.PublicNodes {
		errs = append(errs, fmt.Errorf("%w: publicNodes is meaningful only for provider gcp", ErrInvalidSpec))
	}
	if len(s.NodePools) == 0 {
		errs = append(errs, fmt.Errorf("%w: at least one node pool is required", ErrInvalidSpec))
	}
	// Subnets is optional on every provider: EnsureNetwork creates a network
	// when none is supplied, the same way Azure already did before AWS and
	// GCP gained the same capability.
	if s.VPCCIDR != "" {
		if _, _, err := net.ParseCIDR(s.VPCCIDR); err != nil {
			errs = append(errs, fmt.Errorf("%w: vpcCIDR %q is not a valid CIDR", ErrInvalidSpec, s.VPCCIDR))
		}
	}
	if s.VNetCIDR != "" {
		if _, _, err := net.ParseCIDR(s.VNetCIDR); err != nil {
			errs = append(errs, fmt.Errorf("%w: vnetCIDR %q is not a valid CIDR", ErrInvalidSpec, s.VNetCIDR))
		}
	}
	if s.SubnetCIDR != "" {
		if _, _, err := net.ParseCIDR(s.SubnetCIDR); err != nil {
			errs = append(errs, fmt.Errorf("%w: subnetCIDR %q is not a valid CIDR", ErrInvalidSpec, s.SubnetCIDR))
		}
	}

	seen := make(map[string]struct{}, len(s.NodePools))
	for _, np := range s.NodePools {
		if err := np.Validate(); err != nil {
			errs = append(errs, err)
		}
		if _, dup := seen[np.Name]; dup && np.Name != "" {
			errs = append(errs, fmt.Errorf("%w: duplicate node pool name %q", ErrInvalidSpec, np.Name))
		}
		seen[np.Name] = struct{}{}
	}

	if !s.Size.Valid() {
		errs = append(errs, fmt.Errorf("%w: size %q must be one of small, medium, large", ErrInvalidSpec, s.Size))
	}

	seenOverride := make(map[string]struct{}, len(s.Overrides))
	for _, o := range s.Overrides {
		if err := o.Validate(); err != nil {
			errs = append(errs, err)
		}
		if _, dup := seenOverride[o.Name]; dup && o.Name != "" {
			errs = append(errs, fmt.Errorf("%w: duplicate override for addon %q", ErrInvalidSpec, o.Name))
		}
		seenOverride[o.Name] = struct{}{}
	}

	return errors.Join(errs...)
}
