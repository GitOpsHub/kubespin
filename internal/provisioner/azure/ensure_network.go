package azure

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

// Defaults used when spec.VNetCIDR/SubnetCIDR are empty. Large enough for a
// single cluster's node pools without colliding with the common 10.0.0.0/8
// private ranges peered networks tend to avoid reusing at this exact slice.
const (
	defaultVNetCIDR   = "10.0.0.0/16"
	defaultSubnetCIDR = "10.0.1.0/24"
)

// EnsureNetwork resolves the subnet(s) the cluster will be created in.
//
// The resource group is ensured unconditionally, first: it is a prerequisite
// for the cluster itself as well as for any network kubespin creates, and
// nothing else in this package creates it. When spec.Subnets is already set,
// EnsureNetwork stops there and passes it through unchanged — the operator
// owns that network. When empty, it creates a VNet and subnet named
// deterministically from the cluster ID, so a resumed or repeated apply
// converges to the same resources rather than creating duplicates.
func (p *NetworkProvisioner) EnsureNetwork(
	ctx context.Context, spec core.ClusterSpec,
) (provisioner.NetworkResult, error) {
	n := names{spec}

	exists, err := p.c.resourceGroups.GetResourceGroup(ctx, n.resourceGroup())
	if err != nil {
		return provisioner.NetworkResult{}, fmt.Errorf("checking resource group %s: %w", n.resourceGroup(), err)
	}
	if !exists {
		if err := p.c.resourceGroups.EnsureResourceGroup(ctx, n.resourceGroup(), spec.Region); err != nil {
			return provisioner.NetworkResult{}, fmt.Errorf("creating resource group %s: %w", n.resourceGroup(), err)
		}
		p.c.logger.Info("created resource group", "group", n.resourceGroup(), "region", spec.Region)
	}

	if len(spec.Subnets) > 0 {
		return provisioner.NetworkResult{SubnetIDs: spec.Subnets}, nil
	}

	var change provisioner.Change

	vnetCIDR := spec.VNetCIDR
	if vnetCIDR == "" {
		vnetCIDR = defaultVNetCIDR
	}
	subnetCIDR := spec.SubnetCIDR
	if subnetCIDR == "" {
		subnetCIDR = defaultSubnetCIDR
	}

	if _, err := p.c.network.GetVirtualNetwork(ctx, n.resourceGroup(), n.vnet()); err != nil {
		if code(err) != 404 {
			return provisioner.NetworkResult{}, fmt.Errorf("describing virtual network %s: %w", n.vnet(), err)
		}

		vnet := armnetwork.VirtualNetwork{
			Location: ptr(spec.Region),
			Tags:     tags(spec),
			Properties: &armnetwork.VirtualNetworkPropertiesFormat{
				AddressSpace: &armnetwork.AddressSpace{
					AddressPrefixes: ptrSlice([]string{vnetCIDR}),
				},
			},
		}
		if err := p.c.network.CreateOrUpdateVirtualNetwork(ctx, n.resourceGroup(), n.vnet(), vnet); err != nil {
			return provisioner.NetworkResult{}, fmt.Errorf("creating virtual network %s: %w", n.vnet(), err)
		}
		p.c.logger.Info("created virtual network", "vnet", n.vnet(), "cidr", vnetCIDR)
		change.Changed = true
		change.Details = append(change.Details, fmt.Sprintf("created virtual network %s (%s)", n.vnet(), vnetCIDR))
	}

	subnetID, err := p.ensureSubnet(ctx, n, subnetCIDR, &change)
	if err != nil {
		return provisioner.NetworkResult{}, err
	}

	return provisioner.NetworkResult{SubnetIDs: []string{subnetID}, Change: change}, nil
}

func (p *NetworkProvisioner) ensureSubnet(
	ctx context.Context, n names, subnetCIDR string, change *provisioner.Change,
) (string, error) {
	if existing, err := p.c.network.GetSubnet(ctx, n.resourceGroup(), n.vnet(), n.subnet()); err == nil {
		return deref(existing.ID), nil
	} else if code(err) != 404 {
		return "", fmt.Errorf("describing subnet %s: %w", n.subnet(), err)
	}

	subnet := armnetwork.Subnet{
		Properties: &armnetwork.SubnetPropertiesFormat{
			AddressPrefix: ptr(subnetCIDR),
		},
	}
	if err := p.c.network.CreateOrUpdateSubnet(ctx, n.resourceGroup(), n.vnet(), n.subnet(), subnet); err != nil {
		return "", fmt.Errorf("creating subnet %s: %w", n.subnet(), err)
	}
	p.c.logger.Info("created subnet", "subnet", n.subnet(), "cidr", subnetCIDR)
	change.Changed = true
	change.Details = append(change.Details, fmt.Sprintf("created subnet %s (%s)", n.subnet(), subnetCIDR))

	created, err := p.c.network.GetSubnet(ctx, n.resourceGroup(), n.vnet(), n.subnet())
	if err != nil {
		return "", fmt.Errorf("describing newly created subnet %s: %w", n.subnet(), err)
	}
	return deref(created.ID), nil
}
