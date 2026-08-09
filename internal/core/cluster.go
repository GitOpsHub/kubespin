// Package core holds the domain types shared by every other package.
//
// It is deliberately dependency-free: no cloud SDKs, no I/O, and no imports of
// other internal packages. Everything else in the tree imports core, so keeping
// it a leaf is what prevents import cycles as the tree grows.
package core

import (
	"errors"
	"fmt"
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

// NodePool is a homogeneous group of worker nodes. Sizing changes here are
// infra diffs: they resolve to a cloud SDK reconcile, never to a git commit.
type NodePool struct {
	Name         string            `yaml:"name" json:"name"`
	InstanceType string            `yaml:"instanceType" json:"instanceType"`
	MinSize      int32             `yaml:"minSize" json:"minSize"`
	MaxSize      int32             `yaml:"maxSize" json:"maxSize"`
	DesiredSize  int32             `yaml:"desiredSize" json:"desiredSize"`
	Labels       map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
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
	return errors.Join(errs...)
}

// ClusterSpec is the desired state of one cluster: the contents of the
// cluster.yaml in that cluster's repository.
type ClusterSpec struct {
	ID                ClusterID  `yaml:"id" json:"id"`
	Provider          Provider   `yaml:"provider" json:"provider"`
	Region            string     `yaml:"region" json:"region"`
	Access            Access     `yaml:"access" json:"access"`
	KubernetesVersion string     `yaml:"kubernetesVersion,omitempty" json:"kubernetesVersion,omitempty"`
	NodePools         []NodePool `yaml:"nodePools" json:"nodePools"`
	Profile           ProfileRef `yaml:"profile" json:"profile"`

	// AuthorizedCIDRs restricts API server access. It is meaningful only for
	// AccessPublic; a private cluster has no public endpoint to restrict.
	AuthorizedCIDRs []string `yaml:"authorizedCIDRs,omitempty" json:"authorizedCIDRs,omitempty"`

	// Subnets place the cluster on an existing network, named in whatever form
	// the provider uses: subnet IDs on AWS, a subnetwork on GCP, a subnet
	// resource ID on Azure.
	//
	// kubespin does not create networks. Network topology is almost always
	// owned by a separate team with its own IP plan, peering, and egress rules,
	// so a cluster tool that invented its own VPCs would fight that ownership
	// rather than fit into it.
	Subnets []string `yaml:"subnets" json:"subnets"`

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
	if len(s.NodePools) == 0 {
		errs = append(errs, fmt.Errorf("%w: at least one node pool is required", ErrInvalidSpec))
	}
	if len(s.Subnets) == 0 {
		errs = append(errs, fmt.Errorf("%w: at least one subnet is required", ErrInvalidSpec))
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

	if err := s.Profile.Validate(); err != nil {
		errs = append(errs, err)
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
