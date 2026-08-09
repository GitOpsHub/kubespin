package gcp

import (
	"context"
	"fmt"

	compute "google.golang.org/api/compute/v1"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

// NetworkProvisioner opens the cluster's outbound path to the ingestion API.
type NetworkProvisioner struct {
	c       *Clients
	cluster *ClusterProvisioner
}

// NewNetworkProvisioner builds an egress provisioner.
func NewNetworkProvisioner(c *Clients) *NetworkProvisioner {
	return &NetworkProvisioner{c: c, cluster: NewClusterProvisioner(c)}
}

// Provider identifies this implementation's cloud.
func (p *NetworkProvisioner) Provider() core.Provider { return core.ProviderGCP }

// Default used when spec.SubnetCIDR is empty.
const defaultSubnetCIDR = "10.0.0.0/20"

// EnsureNetwork resolves the subnetwork the cluster will be created in.
//
// When spec.Subnets is already set, EnsureNetwork passes it through
// unchanged — the operator owns that network. When empty, it creates a
// custom-mode VPC network and a single subnetwork in the cluster's region,
// named deterministically from the cluster ID so a resumed or repeated apply
// converges to the same resources rather than creating duplicates.
func (p *NetworkProvisioner) EnsureNetwork(
	ctx context.Context, spec core.ClusterSpec,
) (provisioner.NetworkResult, error) {
	if len(spec.Subnets) > 0 {
		return provisioner.NetworkResult{SubnetIDs: spec.Subnets}, nil
	}

	n := names{project: p.c.project, spec: spec}
	var change provisioner.Change

	if err := p.ensureVPCNetwork(ctx, n, &change); err != nil {
		return provisioner.NetworkResult{}, err
	}

	subnetCIDR := spec.SubnetCIDR
	if subnetCIDR == "" {
		subnetCIDR = defaultSubnetCIDR
	}
	if err := p.ensureSubnetwork(ctx, n, subnetCIDR, &change); err != nil {
		return provisioner.NetworkResult{}, err
	}

	return provisioner.NetworkResult{SubnetIDs: []string{n.subnetworkResource()}, Change: change}, nil
}

func (p *NetworkProvisioner) ensureVPCNetwork(
	ctx context.Context, n names, change *provisioner.Change,
) error {
	name := n.network()

	if _, err := p.c.networks.GetNetwork(ctx, n.project, name); err == nil {
		return nil
	} else if code(err) != 404 {
		return fmt.Errorf("describing network %s: %w", name, err)
	}

	err := p.c.networks.InsertNetwork(ctx, n.project, &compute.Network{
		Name:                  name,
		AutoCreateSubnetworks: false,
		Description:           "kubespin-managed network for cluster " + n.spec.ID.String(),
	})
	if err != nil {
		if code(err) == 409 {
			return nil
		}
		return fmt.Errorf("creating network %s: %w", name, err)
	}

	change.Changed = true
	change.Details = append(change.Details, fmt.Sprintf("created network %s", name))
	return nil
}

func (p *NetworkProvisioner) ensureSubnetwork(
	ctx context.Context, n names, cidr string, change *provisioner.Change,
) error {
	name := n.subnetwork()

	if _, err := p.c.subnetworks.GetSubnetwork(ctx, n.project, n.location(), name); err == nil {
		return nil
	} else if code(err) != 404 {
		return fmt.Errorf("describing subnetwork %s: %w", name, err)
	}

	err := p.c.subnetworks.InsertSubnetwork(ctx, n.project, n.location(), &compute.Subnetwork{
		Name:                  name,
		Network:               n.networkResource(),
		IpCidrRange:           cidr,
		Region:                n.location(),
		PrivateIpGoogleAccess: true,
	})
	if err != nil {
		if code(err) == 409 {
			return nil
		}
		return fmt.Errorf("creating subnetwork %s: %w", name, err)
	}

	change.Changed = true
	change.Details = append(change.Details, fmt.Sprintf("created subnetwork %s (%s)", name, cidr))
	return nil
}

// AllowEgress authorises outbound traffic from the cluster's network to the
// ingestion endpoint via a VPC firewall rule.
//
// This is the only route fleet state has out of a cluster. It is idempotent —
// an existing rule with this cluster's name is left alone — so a resumed or
// repeated apply does not accumulate duplicates.
func (p *NetworkProvisioner) AllowEgress(
	ctx context.Context, spec core.ClusterSpec, dest provisioner.EgressDestination,
) (provisioner.Change, error) {
	var change provisioner.Change

	state, err := p.cluster.Describe(ctx, spec)
	if err != nil {
		return change, err
	}
	if state.NetworkID == "" {
		return change, fmt.Errorf("%w: %s has no network yet", provisioner.ErrNotFound, spec.ID)
	}

	name := "kubespin-" + spec.ID.String() + "-egress"

	if _, err := p.c.firewalls.GetFirewall(ctx, p.c.project, name); err == nil {
		return change, nil
	} else if code(err) != 404 {
		return change, fmt.Errorf("describing firewall rule %s: %w", name, err)
	}

	cidr := dest.CIDR
	if cidr == "" {
		cidr = "0.0.0.0/0"
	}
	port := dest.Port
	if port == 0 {
		port = 443
	}

	description := dest.Description
	if description == "" {
		description = "kubespin fleet-status-reporter egress"
	}

	err = p.c.firewalls.Insert(ctx, p.c.project, &compute.Firewall{
		Name:              name,
		Network:           state.NetworkID,
		Direction:         "EGRESS",
		DestinationRanges: []string{cidr},
		Allowed: []*compute.FirewallAllowed{{
			IPProtocol: "tcp",
			Ports:      []string{fmt.Sprintf("%d", port)},
		}},
		Description: description,
		TargetTags:  []string{spec.ID.String()},
	})
	if err != nil {
		if code(err) == 409 {
			return change, nil
		}
		return change, fmt.Errorf("authorising egress from %s to %s:%d: %w",
			state.NetworkID, cidr, port, err)
	}

	change.Changed = true
	change.Details = append(change.Details,
		fmt.Sprintf("allow egress to %s:%d for %s", cidr, port, dest.Host))
	return change, nil
}
