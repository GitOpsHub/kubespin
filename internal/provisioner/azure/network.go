package azure

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"

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
func (p *NetworkProvisioner) Provider() core.Provider { return core.ProviderAzure }

// AllowEgress authorises outbound traffic from the cluster's node network
// security group to the ingestion endpoint.
//
// This is the only route fleet state has out of a cluster. AKS places the
// cluster's NSG in its node resource group rather than exposing a fixed name,
// so this looks it up rather than assuming one; it is idempotent — an
// existing rule with this cluster's name is left alone — so a resumed or
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
		return change, fmt.Errorf("%w: %s has no node resource group yet", provisioner.ErrNotFound, spec.ID)
	}

	nsg, err := p.findSecurityGroup(ctx, state.NetworkID)
	if err != nil {
		return change, err
	}
	if nsg == "" {
		return change, fmt.Errorf("%w: no network security group found in %s yet",
			provisioner.ErrNotFound, state.NetworkID)
	}

	n := names{spec}

	if _, err := p.c.network.GetSecurityRule(ctx, state.NetworkID, nsg, n.securityRule()); err == nil {
		return change, nil
	} else if code(err) != 404 {
		return change, fmt.Errorf("describing security rule %s: %w", n.securityRule(), err)
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

	rule := armnetwork.SecurityRule{
		Properties: &armnetwork.SecurityRulePropertiesFormat{
			Access:                   ptr(armnetwork.SecurityRuleAccessAllow),
			Direction:                ptr(armnetwork.SecurityRuleDirectionOutbound),
			Protocol:                 ptr(armnetwork.SecurityRuleProtocolTCP),
			Priority:                 ptr(int32(200)),
			SourceAddressPrefix:      ptr("*"),
			SourcePortRange:          ptr("*"),
			DestinationAddressPrefix: ptr(cidr),
			DestinationPortRange:     ptr(fmt.Sprintf("%d", port)),
			Description:              ptr(description),
		},
	}

	if err := p.c.network.CreateOrUpdateSecurityRule(ctx, state.NetworkID, nsg, n.securityRule(), rule); err != nil {
		return change, fmt.Errorf("authorising egress from %s to %s:%d: %w", nsg, cidr, port, err)
	}

	p.c.logger.Info("opened egress security rule", "cluster", spec.ID, "nsg", nsg, "cidr", cidr, "port", port, "destination", dest.Host)
	change.Changed = true
	change.Details = append(change.Details,
		fmt.Sprintf("allow egress to %s:%d for %s", cidr, port, dest.Host))
	return change, nil
}

// findSecurityGroup returns the name of the cluster's NSG within its node
// resource group, or "" if AKS has not created one yet.
func (p *NetworkProvisioner) findSecurityGroup(ctx context.Context, nodeResourceGroup string) (string, error) {
	groups, err := p.c.network.ListSecurityGroups(ctx, nodeResourceGroup)
	if err != nil {
		return "", fmt.Errorf("listing network security groups in %s: %w", nodeResourceGroup, err)
	}
	if len(groups) == 0 {
		return "", nil
	}
	return deref(groups[0].Name), nil
}
