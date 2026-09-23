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

// NetworkProvisioner resolves, creates, and deletes the VPC a cluster lives in.
type NetworkProvisioner struct {
	c *Clients

	// retryInterval defaults to deleteVPCRetryInterval, overridden to zero in
	// tests so DeleteNetwork's DependencyViolation retry doesn't actually sleep.
	retryInterval time.Duration
}

// NewNetworkProvisioner builds a VPC provisioner.
func NewNetworkProvisioner(c *Clients) *NetworkProvisioner {
	return &NetworkProvisioner{c: c, retryInterval: deleteVPCRetryInterval}
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
		// An earlier run may have been interrupted between CreateVpc and
		// enabling DNS, so an adopted VPC is converged too, not trusted.
		vpcID := aws.ToString(out.Vpcs[0].VpcId)
		return vpcID, p.ensureVPCDNS(ctx, vpcID, change)
	}

	created, err := p.c.ec2.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock:         aws.String(cidr),
		TagSpecifications: tagSpec(ec2types.ResourceTypeVpc, name, n.spec),
	})
	if err != nil {
		return "", fmt.Errorf("creating VPC %s: %w", name, err)
	}
	vpcID := aws.ToString(created.Vpc.VpcId)

	p.c.logger.Debug("Created VPC", "vpc", vpcID, "cidr", cidr)
	record(change, fmt.Sprintf("created VPC %s (%s)", vpcID, cidr))
	return vpcID, p.ensureVPCDNS(ctx, vpcID, change)
}

// ensureVPCDNS turns on DNS support and DNS hostnames, which EKS requires and
// which a new VPC does not have on by default. Each is read first, so a
// converged VPC costs no write.
func (p *NetworkProvisioner) ensureVPCDNS(ctx context.Context, vpcID string, change *provisioner.Change) error {
	for _, attr := range []ec2types.VpcAttributeName{
		ec2types.VpcAttributeNameEnableDnsSupport,
		ec2types.VpcAttributeNameEnableDnsHostnames,
	} {
		out, err := p.c.ec2.DescribeVpcAttribute(ctx, &ec2.DescribeVpcAttributeInput{
			VpcId:     aws.String(vpcID),
			Attribute: attr,
		})
		if err != nil {
			return fmt.Errorf("reading %s on %s: %w", attr, vpcID, err)
		}

		input := &ec2.ModifyVpcAttributeInput{VpcId: aws.String(vpcID)}
		enabled := &ec2types.AttributeBooleanValue{Value: aws.Bool(true)}
		if attr == ec2types.VpcAttributeNameEnableDnsSupport {
			if out.EnableDnsSupport != nil && aws.ToBool(out.EnableDnsSupport.Value) {
				continue
			}
			input.EnableDnsSupport = enabled
		} else {
			if out.EnableDnsHostnames != nil && aws.ToBool(out.EnableDnsHostnames.Value) {
				continue
			}
			input.EnableDnsHostnames = enabled
		}

		if _, err := p.c.ec2.ModifyVpcAttribute(ctx, input); err != nil {
			return fmt.Errorf("enabling %s on %s: %w", attr, vpcID, err)
		}
		record(change, fmt.Sprintf("enabled %s on VPC %s", attr, vpcID))
	}
	return nil
}

// availabilityZones lists the region's available zones, sorted so the pair
// picked for the two subnets is deterministic across runs.
func (p *NetworkProvisioner) availabilityZones(ctx context.Context) ([]string, error) {
	out, err := p.c.ec2.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{
		// zone-type keeps out Local and Wavelength Zones an account has opted
		// into: EKS rejects a control plane subnet in either, and their
		// names ("us-west-2-lax-1a") sort ahead of the region's own zones.
		Filters: []ec2types.Filter{
			{Name: aws.String("state"), Values: []string{"available"}},
			{Name: aws.String("zone-type"), Values: []string{"availability-zone"}},
		},
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
		subnet := out.Subnets[0]
		subnetID := aws.ToString(subnet.SubnetId)
		if aws.ToBool(subnet.MapPublicIpOnLaunch) {
			return subnetID, nil
		}
		// Created by a run interrupted before the attribute below was set.
		return subnetID, p.enablePublicIPOnLaunch(ctx, name, subnetID, change)
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

	p.c.logger.Debug("Created Subnet", "subnet", name, "cidr", cidr)
	record(change, fmt.Sprintf("created subnet %s (%s)", name, cidr))
	return subnetID, p.enablePublicIPOnLaunch(ctx, name, subnetID, change)
}

// enablePublicIPOnLaunch is required because there is no NAT gateway on this
// network (see CLAUDE.md's AWS network invariant: IGW + route table only):
// without an auto-assigned public IP, nodes launched into the subnet have no
// route out to the internet at all and can never join the cluster.
func (p *NetworkProvisioner) enablePublicIPOnLaunch(
	ctx context.Context, name, subnetID string, change *provisioner.Change,
) error {
	if _, err := p.c.ec2.ModifySubnetAttribute(ctx, &ec2.ModifySubnetAttributeInput{
		SubnetId:            aws.String(subnetID),
		MapPublicIpOnLaunch: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
	}); err != nil {
		return fmt.Errorf("enabling auto-assign public IP for subnet %s: %w", name, err)
	}
	record(change, fmt.Sprintf("enabled auto-assign public IP on subnet %s", name))
	return nil
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
		igw := out.InternetGateways[0]
		igwID := aws.ToString(igw.InternetGatewayId)
		for _, attachment := range igw.Attachments {
			if aws.ToString(attachment.VpcId) == vpcID {
				return igwID, nil
			}
		}
		// Created by a run interrupted before it was attached.
		return igwID, p.attachInternetGateway(ctx, igwID, vpcID, change)
	}

	created, err := p.c.ec2.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{
		TagSpecifications: tagSpec(ec2types.ResourceTypeInternetGateway, name, n.spec),
	})
	if err != nil {
		return "", fmt.Errorf("creating internet gateway %s: %w", name, err)
	}
	igwID := aws.ToString(created.InternetGateway.InternetGatewayId)

	p.c.logger.Debug("Created Internet Gateway", "gateway", igwID)
	record(change, fmt.Sprintf("created internet gateway %s", igwID))
	return igwID, p.attachInternetGateway(ctx, igwID, vpcID, change)
}

func (p *NetworkProvisioner) attachInternetGateway(
	ctx context.Context, igwID, vpcID string, change *provisioner.Change,
) error {
	if _, err := p.c.ec2.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String(vpcID),
	}); err != nil {
		return fmt.Errorf("attaching internet gateway %s to %s: %w", igwID, vpcID, err)
	}
	record(change, fmt.Sprintf("attached internet gateway %s to %s", igwID, vpcID))
	return nil
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
	// An existing table is converged rather than trusted: a run interrupted
	// between CreateRouteTable and the calls below leaves a table with no
	// default route or no subnet associations, and nodes that can never join.
	var table ec2types.RouteTable
	if len(out.RouteTables) > 0 {
		table = out.RouteTables[0]
	} else {
		created, err := p.c.ec2.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{
			VpcId:             aws.String(vpcID),
			TagSpecifications: tagSpec(ec2types.ResourceTypeRouteTable, name, n.spec),
		})
		if err != nil {
			return fmt.Errorf("creating route table %s: %w", name, err)
		}
		table = *created.RouteTable
		p.c.logger.Debug("Created Route Table", "table", aws.ToString(table.RouteTableId))
		record(change, fmt.Sprintf("created route table %s", aws.ToString(table.RouteTableId)))
	}
	rtID := aws.ToString(table.RouteTableId)

	if !hasDefaultRoute(table) {
		if _, err := p.c.ec2.CreateRoute(ctx, &ec2.CreateRouteInput{
			RouteTableId:         aws.String(rtID),
			DestinationCidrBlock: aws.String(defaultRouteCIDR),
			GatewayId:            aws.String(igwID),
		}); err != nil {
			return fmt.Errorf("adding default route to %s: %w", rtID, err)
		}
		record(change, fmt.Sprintf("added default route to %s via %s", rtID, igwID))
	}

	for _, subnetID := range subnetIDs {
		if isAssociated(table, subnetID) {
			continue
		}
		if _, err := p.c.ec2.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
			RouteTableId: aws.String(rtID),
			SubnetId:     aws.String(subnetID),
		}); err != nil {
			return fmt.Errorf("associating route table %s with subnet %s: %w", rtID, subnetID, err)
		}
		record(change, fmt.Sprintf("associated route table %s with subnet %s", rtID, subnetID))
	}
	return nil
}

const defaultRouteCIDR = "0.0.0.0/0"

func hasDefaultRoute(table ec2types.RouteTable) bool {
	for _, route := range table.Routes {
		if aws.ToString(route.DestinationCidrBlock) == defaultRouteCIDR {
			return true
		}
	}
	return false
}

func isAssociated(table ec2types.RouteTable, subnetID string) bool {
	for _, assoc := range table.Associations {
		if aws.ToString(assoc.SubnetId) == subnetID {
			return true
		}
	}
	return false
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
		p.c.logger.Info("Deleted Security Group", "group", groupID)
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
		p.c.logger.Info("Deleted Internet Gateway", "gateway", igwID)
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
		p.c.logger.Info("Deleted Subnet", "subnet", subnetID)
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
		p.c.logger.Info("Deleted Route Table", "table", rtID)
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
			p.c.logger.Info("Deleted VPC", "vpc", vpcID)
			return nil
		}
		if !isAWSErrorCode(err, "DependencyViolation") {
			return fmt.Errorf("deleting VPC %s: %w", vpcID, err)
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return fmt.Errorf("deleting VPC %s: %w", vpcID, ctx.Err())
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
