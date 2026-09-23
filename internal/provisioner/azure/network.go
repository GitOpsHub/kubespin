package azure

import (
	"context"
	"fmt"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// NetworkProvisioner resolves, creates, and deletes the VNet a cluster lives in.
type NetworkProvisioner struct {
	c       *Clients
	cluster *ClusterProvisioner
}

// NewNetworkProvisioner builds a VNet provisioner.
func NewNetworkProvisioner(c *Clients) *NetworkProvisioner {
	return &NetworkProvisioner{c: c, cluster: NewClusterProvisioner(c)}
}

// Provider identifies this implementation's cloud.
func (p *NetworkProvisioner) Provider() core.Provider { return core.ProviderAzure }

// DeleteNetwork reverses the resource group EnsureNetwork unconditionally
// creates: it deletes n.resourceGroup() and everything ARM still finds inside
// it (the AKS cluster resource is already gone by the time Teardown calls
// this; the VNet/subnet are only there if kubespin created them). If the
// group does not exist — already torn down, or never created because
// EnsureNetwork itself failed before this cluster got anywhere — this is a
// no-op. There is no operator-supplied-network case to protect here the way
// AWS/GCP do: unlike a VPC or a VPC network, this resource group holds the
// AKS cluster resource itself, so kubespin always owns it regardless of
// whether --subnets pointed at a VNet the operator owns.
func (p *NetworkProvisioner) DeleteNetwork(ctx context.Context, spec core.ClusterSpec) error {
	n := names{spec}

	exists, err := p.c.resourceGroups.GetResourceGroup(ctx, n.resourceGroup())
	if err != nil {
		return fmt.Errorf("checking resource group %s: %w", n.resourceGroup(), err)
	}
	if !exists {
		return nil
	}

	if err := p.c.resourceGroups.DeleteResourceGroup(ctx, n.resourceGroup()); err != nil {
		return fmt.Errorf("deleting resource group %s: %w", n.resourceGroup(), err)
	}
	p.c.logger.Info("Deleted Resource Group", "group", n.resourceGroup())
	return nil
}
