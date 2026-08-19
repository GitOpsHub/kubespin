package aws

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

// Defaults used when spec.VPCCIDR is empty.
const (
	defaultVPCCIDR = "10.0.0.0/16"
	// subnetPrefixLen sizes each carved subnet. /24 fits comfortably inside
	// the default /16 twice over with room for more, and inside any
	// operator-supplied CIDR at least /24 wide.
	subnetPrefixLen = 24
	// subnetsWanted is the EKS control plane's minimum: it requires subnets
	// in at least two distinct Availability Zones.
	subnetsWanted = 2
)

// NetworkProvisioner opens the cluster's outbound path to the ingestion API.
type NetworkProvisioner struct {
	c       *Clients
	cluster *ClusterProvisioner

	// retryInterval defaults to deleteVPCRetryInterval, overridden to zero in
	// tests so DeleteNetwork's DependencyViolation retry doesn't actually sleep.
	retryInterval time.Duration
}

// NewNetworkProvisioner builds an egress provisioner.
func NewNetworkProvisioner(c *Clients) *NetworkProvisioner {
	return &NetworkProvisioner{c: c, cluster: NewClusterProvisioner(c), retryInterval: deleteVPCRetryInterval}
}

// Provider identifies this implementation's cloud.
func (p *NetworkProvisioner) Provider() core.Provider { return core.ProviderAWS }

// EnsureNetwork resolves the subnet(s) the cluster will be created in.
//
// When spec.Subnets is already set, EnsureNetwork passes it through
// unchanged — the operator owns that network. When empty, it creates a VPC
// with two subnets across two Availability Zones (EKS requires at least two
// for its control plane), an Internet Gateway, and a public route table, all
// named deterministically from the cluster ID so a resumed or repeated apply
// converges to the same resources rather than creating duplicates.
func (p *NetworkProvisioner) EnsureNetwork(
	ctx context.Context, spec core.ClusterSpec,
) (provisioner.NetworkResult, error) {
	if len(spec.Subnets) > 0 {
		return provisioner.NetworkResult{SubnetIDs: spec.Subnets}, nil
	}

	n := names{spec}
	var change provisioner.Change

	vpcCIDR := spec.VPCCIDR
	if vpcCIDR == "" {
		vpcCIDR = defaultVPCCIDR
	}

	vpcID, err := p.ensureVPC(ctx, n, vpcCIDR, &change)
	if err != nil {
		return provisioner.NetworkResult{}, err
	}

	azs, err := p.availabilityZones(ctx)
	if err != nil {
		return provisioner.NetworkResult{}, err
	}
	if len(azs) < subnetsWanted {
		return provisioner.NetworkResult{}, fmt.Errorf(
			"region %s has fewer than %d availability zones", spec.Region, subnetsWanted)
	}

	subnetIDs := make([]string, 0, subnetsWanted)
	for i := 0; i < subnetsWanted; i++ {
		cidr, err := carveSubnetCIDR(vpcCIDR, i)
		if err != nil {
			return provisioner.NetworkResult{}, fmt.Errorf("computing subnet CIDR for %s: %w", n.vpcName(), err)
		}
		subnetID, err := p.ensureSubnet(ctx, n, vpcID, azs[i], cidr, &change)
		if err != nil {
			return provisioner.NetworkResult{}, err
		}
		subnetIDs = append(subnetIDs, subnetID)
	}

	igwID, err := p.ensureInternetGateway(ctx, n, vpcID, &change)
	if err != nil {
		return provisioner.NetworkResult{}, err
	}

	if err := p.ensureRouteTable(ctx, n, vpcID, igwID, subnetIDs, &change); err != nil {
		return provisioner.NetworkResult{}, err
	}

	return provisioner.NetworkResult{SubnetIDs: subnetIDs, Change: change}, nil
}

func (p *NetworkProvisioner) ensureVPC(
	ctx context.Context, n names, cidr string, change *provisioner.Change,
) (string, error) {
	name := n.vpcName()

	out, err := p.c.ec2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []ec2types.Filter{tagNameFilter(name)},
	})
	if err != nil {
		return "", fmt.Errorf("describing VPC %s: %w", name, err)
	}
	if len(out.Vpcs) > 0 {
		return aws.ToString(out.Vpcs[0].VpcId), nil
	}

	created, err := p.c.ec2.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock:         aws.String(cidr),
		TagSpecifications: tagSpec(ec2types.ResourceTypeVpc, name, n.spec),
	})
	if err != nil {
		return "", fmt.Errorf("creating VPC %s: %w", name, err)
	}
	vpcID := aws.ToString(created.Vpc.VpcId)

	// EKS requires both; neither defaults on for a newly created VPC.
	if _, err := p.c.ec2.ModifyVpcAttribute(ctx, &ec2.ModifyVpcAttributeInput{
		VpcId:            aws.String(vpcID),
		EnableDnsSupport: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
	}); err != nil {
		return "", fmt.Errorf("enabling DNS support on %s: %w", vpcID, err)
	}
	if _, err := p.c.ec2.ModifyVpcAttribute(ctx, &ec2.ModifyVpcAttributeInput{
		VpcId:              aws.String(vpcID),
		EnableDnsHostnames: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
	}); err != nil {
		return "", fmt.Errorf("enabling DNS hostnames on %s: %w", vpcID, err)
	}

	p.c.logger.Info("created VPC", "vpc", vpcID, "cidr", cidr)
	change.Changed = true
	change.Details = append(change.Details, fmt.Sprintf("created VPC %s (%s)", vpcID, cidr))
	return vpcID, nil
}

// availabilityZones lists the region's available zones, sorted so the pair
// picked for the two subnets is deterministic across runs.
func (p *NetworkProvisioner) availabilityZones(ctx context.Context) ([]string, error) {
	out, err := p.c.ec2.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{
		Filters: []ec2types.Filter{{Name: aws.String("state"), Values: []string{"available"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("describing availability zones: %w", err)
	}

	names := make([]string, 0, len(out.AvailabilityZones))
	for _, az := range out.AvailabilityZones {
		names = append(names, aws.ToString(az.ZoneName))
	}
	sort.Strings(names)
	return names, nil
}

func (p *NetworkProvisioner) ensureSubnet(
	ctx context.Context, n names, vpcID, az, cidr string, change *provisioner.Change,
) (string, error) {
	name := n.subnetName(az)

	out, err := p.c.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{tagNameFilter(name)},
	})
	if err != nil {
		return "", fmt.Errorf("describing subnet %s: %w", name, err)
	}
	if len(out.Subnets) > 0 {
		return aws.ToString(out.Subnets[0].SubnetId), nil
	}

	created, err := p.c.ec2.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:             aws.String(vpcID),
		CidrBlock:         aws.String(cidr),
		AvailabilityZone:  aws.String(az),
		TagSpecifications: tagSpec(ec2types.ResourceTypeSubnet, name, n.spec),
	})
	if err != nil {
		return "", fmt.Errorf("creating subnet %s: %w", name, err)
	}
	subnetID := aws.ToString(created.Subnet.SubnetId)

	// There is no NAT gateway on this network (see CLAUDE.md's AWS network
	// invariant: IGW + route table only), so without an auto-assigned public
	// IP, nodes launched into this subnet have no route out to the internet
	// at all and can never join the cluster.
	if _, err := p.c.ec2.ModifySubnetAttribute(ctx, &ec2.ModifySubnetAttributeInput{
		SubnetId:            aws.String(subnetID),
		MapPublicIpOnLaunch: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
	}); err != nil {
		return "", fmt.Errorf("enabling auto-assign public IP for subnet %s: %w", name, err)
	}

	p.c.logger.Info("created subnet", "subnet", name, "cidr", cidr)
	change.Changed = true
	change.Details = append(change.Details, fmt.Sprintf("created subnet %s (%s)", name, cidr))
	return subnetID, nil
}

func (p *NetworkProvisioner) ensureInternetGateway(
	ctx context.Context, n names, vpcID string, change *provisioner.Change,
) (string, error) {
	name := n.igwName()

	out, err := p.c.ec2.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{
		Filters: []ec2types.Filter{tagNameFilter(name)},
	})
	if err != nil {
		return "", fmt.Errorf("describing internet gateway %s: %w", name, err)
	}
	if len(out.InternetGateways) > 0 {
		return aws.ToString(out.InternetGateways[0].InternetGatewayId), nil
	}

	created, err := p.c.ec2.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{
		TagSpecifications: tagSpec(ec2types.ResourceTypeInternetGateway, name, n.spec),
	})
	if err != nil {
		return "", fmt.Errorf("creating internet gateway %s: %w", name, err)
	}
	igwID := aws.ToString(created.InternetGateway.InternetGatewayId)

	if _, err := p.c.ec2.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String(vpcID),
	}); err != nil {
		return "", fmt.Errorf("attaching internet gateway %s to %s: %w", igwID, vpcID, err)
	}

	p.c.logger.Info("created internet gateway", "gateway", igwID)
	change.Changed = true
	change.Details = append(change.Details, fmt.Sprintf("created internet gateway %s", igwID))
	return igwID, nil
}

// ensureRouteTable creates a single public route table shared by both
// subnets. Splitting the two into private subnets behind a NAT gateway is
// out of scope: it is expensive to run and to test, and nothing in the
// architecture requires nodes to be unreachable from their own VPC's egress
// path, only that nothing reaches in from outside it.
func (p *NetworkProvisioner) ensureRouteTable(
	ctx context.Context, n names, vpcID, igwID string, subnetIDs []string, change *provisioner.Change,
) error {
	name := n.routeTableName()

	out, err := p.c.ec2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: []ec2types.Filter{tagNameFilter(name)},
	})
	if err != nil {
		return fmt.Errorf("describing route table %s: %w", name, err)
	}
	if len(out.RouteTables) > 0 {
		// Already created (and associated) by an earlier run.
		return nil
	}

	created, err := p.c.ec2.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{
		VpcId:             aws.String(vpcID),
		TagSpecifications: tagSpec(ec2types.ResourceTypeRouteTable, name, n.spec),
	})
	if err != nil {
		return fmt.Errorf("creating route table %s: %w", name, err)
	}
	rtID := aws.ToString(created.RouteTable.RouteTableId)

	if _, err := p.c.ec2.CreateRoute(ctx, &ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(igwID),
	}); err != nil {
		return fmt.Errorf("adding default route to %s: %w", rtID, err)
	}

	for _, subnetID := range subnetIDs {
		if _, err := p.c.ec2.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
			RouteTableId: aws.String(rtID),
			SubnetId:     aws.String(subnetID),
		}); err != nil {
			return fmt.Errorf("associating route table %s with subnet %s: %w", rtID, subnetID, err)
		}
	}

	p.c.logger.Info("created route table", "table", rtID, "gateway", igwID)
	change.Changed = true
	change.Details = append(change.Details,
		fmt.Sprintf("created route table %s with default route via %s", rtID, igwID))
	return nil
}

// deleteVPCRetries/deleteVPCRetryInterval bound how long DeleteNetwork waits
// out a transient DependencyViolation on the final DeleteVpc call: an ENI a
// just-deleted load balancer owned can take tens of seconds to detach after
// the load balancer itself reports deleted, and every other resource this
// function tears down is already gone by the time it gets here.
const (
	deleteVPCRetries       = 12
	deleteVPCRetryInterval = 10 * time.Second
)

// DeleteNetwork reverses EnsureNetwork: it finds the VPC by the same
// deterministic Name tag EnsureNetwork looked it up by and, if found, deletes
// every resource this package could have created inside it. If no such VPC
// exists — an operator-supplied --subnets network, or one already torn
// down — this is a no-op, never touching a network kubespin did not create.
func (p *NetworkProvisioner) DeleteNetwork(ctx context.Context, spec core.ClusterSpec) error {
	n := names{spec}

	vpcID, err := p.findVPC(ctx, n)
	if err != nil {
		return err
	}
	if vpcID == "" {
		return nil
	}

	if err := p.deleteSecurityGroups(ctx, vpcID); err != nil {
		return err
	}
	if err := p.deleteInternetGateways(ctx, vpcID); err != nil {
		return err
	}
	if err := p.deleteSubnets(ctx, vpcID); err != nil {
		return err
	}
	if err := p.deleteRouteTables(ctx, vpcID); err != nil {
		return err
	}
	return p.deleteVPC(ctx, vpcID)
}

func (p *NetworkProvisioner) findVPC(ctx context.Context, n names) (string, error) {
	out, err := p.c.ec2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []ec2types.Filter{tagNameFilter(n.vpcName())},
	})
	if err != nil {
		return "", fmt.Errorf("describing VPC %s: %w", n.vpcName(), err)
	}
	if len(out.Vpcs) == 0 {
		return "", nil
	}
	return aws.ToString(out.Vpcs[0].VpcId), nil
}

// deleteSecurityGroups removes every non-default group in the VPC. The
// default group is deleted automatically with the VPC itself and cannot be
// deleted directly. A group created outside EnsureNetwork — most notably the
// one a Kubernetes Service of type LoadBalancer's cloud-provider integration
// creates — would otherwise block DeleteVpc the same way it blocked it during
// manual cleanup that motivated this method.
func (p *NetworkProvisioner) deleteSecurityGroups(ctx context.Context, vpcID string) error {
	out, err := p.c.ec2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2types.Filter{vpcIDFilter(vpcID)},
	})
	if err != nil {
		return fmt.Errorf("describing security groups in %s: %w", vpcID, err)
	}
	for _, sg := range out.SecurityGroups {
		if aws.ToString(sg.GroupName) == "default" {
			continue
		}
		groupID := aws.ToString(sg.GroupId)
		if _, err := p.c.ec2.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(groupID),
		}); err != nil {
			return fmt.Errorf("deleting security group %s: %w", groupID, err)
		}
		p.c.logger.Info("deleted security group", "group", groupID)
	}
	return nil
}

func (p *NetworkProvisioner) deleteInternetGateways(ctx context.Context, vpcID string) error {
	out, err := p.c.ec2.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{
		Filters: []ec2types.Filter{{Name: aws.String("attachment.vpc-id"), Values: []string{vpcID}}},
	})
	if err != nil {
		return fmt.Errorf("describing internet gateways attached to %s: %w", vpcID, err)
	}
	for _, igw := range out.InternetGateways {
		igwID := aws.ToString(igw.InternetGatewayId)
		if _, err := p.c.ec2.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(igwID),
			VpcId:             aws.String(vpcID),
		}); err != nil {
			return fmt.Errorf("detaching internet gateway %s: %w", igwID, err)
		}
		if _, err := p.c.ec2.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{
			InternetGatewayId: aws.String(igwID),
		}); err != nil {
			return fmt.Errorf("deleting internet gateway %s: %w", igwID, err)
		}
		p.c.logger.Info("deleted internet gateway", "gateway", igwID)
	}
	return nil
}

func (p *NetworkProvisioner) deleteSubnets(ctx context.Context, vpcID string) error {
	out, err := p.c.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{vpcIDFilter(vpcID)},
	})
	if err != nil {
		return fmt.Errorf("describing subnets in %s: %w", vpcID, err)
	}
	for _, subnet := range out.Subnets {
		subnetID := aws.ToString(subnet.SubnetId)
		if _, err := p.c.ec2.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)}); err != nil {
			return fmt.Errorf("deleting subnet %s: %w", subnetID, err)
		}
		p.c.logger.Info("deleted subnet", "subnet", subnetID)
	}
	return nil
}

// deleteRouteTables skips the main table: it is deleted implicitly with the
// VPC and, unlike the ones EnsureNetwork creates, cannot be deleted directly.
func (p *NetworkProvisioner) deleteRouteTables(ctx context.Context, vpcID string) error {
	out, err := p.c.ec2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: []ec2types.Filter{vpcIDFilter(vpcID)},
	})
	if err != nil {
		return fmt.Errorf("describing route tables in %s: %w", vpcID, err)
	}
	for _, rt := range out.RouteTables {
		if isMainRouteTable(rt) {
			continue
		}
		rtID := aws.ToString(rt.RouteTableId)
		if _, err := p.c.ec2.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{RouteTableId: aws.String(rtID)}); err != nil {
			return fmt.Errorf("deleting route table %s: %w", rtID, err)
		}
		p.c.logger.Info("deleted route table", "table", rtID)
	}
	return nil
}

func isMainRouteTable(rt ec2types.RouteTable) bool {
	for _, assoc := range rt.Associations {
		if aws.ToBool(assoc.Main) {
			return true
		}
	}
	return false
}

// deleteVPC retries a DependencyViolation for a while: an ENI a just-deleted
// load balancer owned can take tens of seconds to detach after the load
// balancer itself reports deleted.
func (p *NetworkProvisioner) deleteVPC(ctx context.Context, vpcID string) error {
	var lastErr error
	for attempt := 0; attempt < deleteVPCRetries; attempt++ {
		_, err := p.c.ec2.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)})
		if err == nil {
			p.c.logger.Info("deleted VPC", "vpc", vpcID)
			return nil
		}
		if !isAWSErrorCode(err, "DependencyViolation") {
			return fmt.Errorf("deleting VPC %s: %w", vpcID, err)
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.retryInterval):
		}
	}
	return fmt.Errorf("deleting VPC %s: dependencies did not clear in time: %w", vpcID, lastErr)
}

func vpcIDFilter(vpcID string) ec2types.Filter {
	return ec2types.Filter{Name: aws.String("vpc-id"), Values: []string{vpcID}}
}

// isAWSErrorCode reports whether err is an AWS API error with the given code
// (e.g. "DependencyViolation"). EC2 surfaces these as smithy.APIError rather
// than typed exceptions the way DynamoDB or EKS do.
func isAWSErrorCode(err error, code string) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == code
}

// tagNameFilter looks resources up by their deterministic Name tag, the same
// discoverability mechanism every EnsureNetwork call in this file relies on
// to adopt existing resources instead of erroring or duplicating.
func tagNameFilter(name string) ec2types.Filter {
	return ec2types.Filter{Name: aws.String("tag:Name"), Values: []string{name}}
}

func tagSpec(resourceType ec2types.ResourceType, name string, spec core.ClusterSpec) []ec2types.TagSpecification {
	base := tags(spec)
	ec2Tags := make([]ec2types.Tag, 0, len(base)+1)
	ec2Tags = append(ec2Tags, ec2types.Tag{Key: aws.String("Name"), Value: aws.String(name)})
	for k, v := range base {
		ec2Tags = append(ec2Tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return []ec2types.TagSpecification{{ResourceType: resourceType, Tags: ec2Tags}}
}

// carveSubnetCIDR derives the index-th /subnetPrefixLen block out of vpcCIDR,
// so two subnets can be sized deterministically from one VPC CIDR without an
// operator having to specify each one separately.
func carveSubnetCIDR(vpcCIDR string, index int) (string, error) {
	_, ipnet, err := net.ParseCIDR(vpcCIDR)
	if err != nil {
		return "", fmt.Errorf("parsing CIDR %s: %w", vpcCIDR, err)
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 {
		return "", fmt.Errorf("CIDR %s is not IPv4", vpcCIDR)
	}
	if ones > subnetPrefixLen {
		return "", fmt.Errorf("CIDR %s is smaller than a /%d, cannot carve subnets", vpcCIDR, subnetPrefixLen)
	}

	if index < 0 {
		return "", fmt.Errorf("subnet index %d must not be negative", index)
	}

	subnetSize := uint32(1) << (32 - subnetPrefixLen)
	base := binary.BigEndian.Uint32(ipnet.IP.To4())
	subnetBase := base + uint32(index)*subnetSize //nolint:gosec // bounds-checked above; caller only ever passes small loop indices (0, 1)

	subnetIP := make(net.IP, 4)
	binary.BigEndian.PutUint32(subnetIP, subnetBase)
	return fmt.Sprintf("%s/%d", subnetIP.String(), subnetPrefixLen), nil
}

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

	p.c.logger.Info("opened egress rule", "cluster", spec.ID, "cidr", cidr, "port", port, "destination", dest.Host)
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
