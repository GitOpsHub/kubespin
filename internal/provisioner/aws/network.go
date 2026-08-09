package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

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
func (p *NetworkProvisioner) Provider() core.Provider { return core.ProviderAWS }

// AllowEgress authorises outbound traffic from the cluster security group to
// the ingestion endpoint.
//
// This is the only route fleet state has out of a cluster. Provisioning it at
// creation rather than later matters: a cluster built without it cannot report
// at all, and fixing that afterwards is a network change per cluster.
//
// It is idempotent — an existing matching rule is left alone — so a resumed or
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
		return change, fmt.Errorf("%w: %s has no cluster security group yet",
			provisioner.ErrNotFound, spec.ID)
	}

	cidr := dest.CIDR
	if cidr == "" {
		cidr = "0.0.0.0/0"
	}
	port := dest.Port
	if port == 0 {
		port = 443
	}

	exists, err := p.egressRuleExists(ctx, state.NetworkID, cidr, port)
	if err != nil {
		return change, err
	}
	if exists {
		return change, nil
	}

	description := dest.Description
	if description == "" {
		description = "kubespin fleet-status-reporter egress"
	}

	_, err = p.c.ec2.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(state.NetworkID),
		IpPermissions: []ec2types.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int32(port),
			ToPort:     aws.Int32(port),
			IpRanges: []ec2types.IpRange{{
				CidrIp:      aws.String(cidr),
				Description: aws.String(description),
			}},
		}},
	})
	if err != nil {
		return change, fmt.Errorf("authorising egress from %s to %s:%d: %w",
			state.NetworkID, cidr, port, err)
	}

	change.Changed = true
	change.Details = append(change.Details,
		fmt.Sprintf("allow egress to %s:%d for %s", cidr, port, dest.Host))
	return change, nil
}

// egressRuleExists reports whether an equivalent rule is already present.
func (p *NetworkProvisioner) egressRuleExists(
	ctx context.Context, groupID, cidr string, port int32,
) (bool, error) {
	out, err := p.c.ec2.DescribeSecurityGroupRules(ctx, &ec2.DescribeSecurityGroupRulesInput{
		Filters: []ec2types.Filter{{
			Name:   aws.String("group-id"),
			Values: []string{groupID},
		}},
	})
	if err != nil {
		return false, fmt.Errorf("describing rules on %s: %w", groupID, err)
	}

	for _, rule := range out.SecurityGroupRules {
		if !aws.ToBool(rule.IsEgress) {
			continue
		}
		if aws.ToString(rule.CidrIpv4) != cidr {
			continue
		}

		// A protocol of "-1" is allow-all, which already covers this
		// destination — adding a narrower duplicate would be noise.
		protocol := aws.ToString(rule.IpProtocol)
		if protocol == "-1" {
			return true, nil
		}
		if protocol != "tcp" {
			continue
		}
		if aws.ToInt32(rule.FromPort) <= port && aws.ToInt32(rule.ToPort) >= port {
			return true, nil
		}
	}
	return false, nil
}
