// Package aws provisions EKS clusters and their networks.
//
// Every AWS service is reached through an interface listing only the calls this
// package makes. That keeps the whole provisioner testable without credentials,
// and doubles as the precise permission set an operator has to grant.
package aws

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/iam"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// eksAPI is the EKS surface this package uses.
type eksAPI interface {
	DescribeCluster(context.Context, *eks.DescribeClusterInput, ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
	CreateCluster(context.Context, *eks.CreateClusterInput, ...func(*eks.Options)) (*eks.CreateClusterOutput, error)
	UpdateClusterConfig(context.Context, *eks.UpdateClusterConfigInput, ...func(*eks.Options)) (*eks.UpdateClusterConfigOutput, error)
	DeleteCluster(context.Context, *eks.DeleteClusterInput, ...func(*eks.Options)) (*eks.DeleteClusterOutput, error)
	ListNodegroups(context.Context, *eks.ListNodegroupsInput, ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error)
	DescribeNodegroup(context.Context, *eks.DescribeNodegroupInput, ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error)
	CreateNodegroup(context.Context, *eks.CreateNodegroupInput, ...func(*eks.Options)) (*eks.CreateNodegroupOutput, error)
	UpdateNodegroupConfig(context.Context, *eks.UpdateNodegroupConfigInput, ...func(*eks.Options)) (*eks.UpdateNodegroupConfigOutput, error)
	DeleteNodegroup(context.Context, *eks.DeleteNodegroupInput, ...func(*eks.Options)) (*eks.DeleteNodegroupOutput, error)
	DescribeAddon(context.Context, *eks.DescribeAddonInput, ...func(*eks.Options)) (*eks.DescribeAddonOutput, error)
	CreateAddon(context.Context, *eks.CreateAddonInput, ...func(*eks.Options)) (*eks.CreateAddonOutput, error)
	UpdateAddon(context.Context, *eks.UpdateAddonInput, ...func(*eks.Options)) (*eks.UpdateAddonOutput, error)
	ListPodIdentityAssociations(context.Context, *eks.ListPodIdentityAssociationsInput, ...func(*eks.Options)) (*eks.ListPodIdentityAssociationsOutput, error)
	CreatePodIdentityAssociation(context.Context, *eks.CreatePodIdentityAssociationInput, ...func(*eks.Options)) (*eks.CreatePodIdentityAssociationOutput, error)
}

// iamAPI covers the service roles EKS needs and each add-on's Pod Identity
// role. The OIDC provider calls only clean up after IRSA-era clusters.
type iamAPI interface {
	GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	CreateRole(context.Context, *iam.CreateRoleInput, ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
	DeleteRole(context.Context, *iam.DeleteRoleInput, ...func(*iam.Options)) (*iam.DeleteRoleOutput, error)
	AttachRolePolicy(context.Context, *iam.AttachRolePolicyInput, ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error)
	ListAttachedRolePolicies(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
	DetachRolePolicy(context.Context, *iam.DetachRolePolicyInput, ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error)
	ListOpenIDConnectProviders(context.Context, *iam.ListOpenIDConnectProvidersInput, ...func(*iam.Options)) (*iam.ListOpenIDConnectProvidersOutput, error)
	GetOpenIDConnectProvider(context.Context, *iam.GetOpenIDConnectProviderInput, ...func(*iam.Options)) (*iam.GetOpenIDConnectProviderOutput, error)
	DeleteOpenIDConnectProvider(context.Context, *iam.DeleteOpenIDConnectProviderInput, ...func(*iam.Options)) (*iam.DeleteOpenIDConnectProviderOutput, error)
	ListInstanceProfilesForRole(context.Context, *iam.ListInstanceProfilesForRoleInput, ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error)
	RemoveRoleFromInstanceProfile(context.Context, *iam.RemoveRoleFromInstanceProfileInput, ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error)
	GetRolePolicy(context.Context, *iam.GetRolePolicyInput, ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error)
	PutRolePolicy(context.Context, *iam.PutRolePolicyInput, ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error)
	ListRolePolicies(context.Context, *iam.ListRolePoliciesInput, ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error)
	DeleteRolePolicy(context.Context, *iam.DeleteRolePolicyInput, ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error)
}

// ec2API covers the VPC/subnets/Internet Gateway/route table EnsureNetwork
// creates when spec.Subnets is empty.
type ec2API interface {
	DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
	CreateVpc(context.Context, *ec2.CreateVpcInput, ...func(*ec2.Options)) (*ec2.CreateVpcOutput, error)
	ModifyVpcAttribute(context.Context, *ec2.ModifyVpcAttributeInput, ...func(*ec2.Options)) (*ec2.ModifyVpcAttributeOutput, error)
	DescribeAvailabilityZones(context.Context, *ec2.DescribeAvailabilityZonesInput, ...func(*ec2.Options)) (*ec2.DescribeAvailabilityZonesOutput, error)
	DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	CreateSubnet(context.Context, *ec2.CreateSubnetInput, ...func(*ec2.Options)) (*ec2.CreateSubnetOutput, error)
	ModifySubnetAttribute(context.Context, *ec2.ModifySubnetAttributeInput, ...func(*ec2.Options)) (*ec2.ModifySubnetAttributeOutput, error)
	DescribeInternetGateways(context.Context, *ec2.DescribeInternetGatewaysInput, ...func(*ec2.Options)) (*ec2.DescribeInternetGatewaysOutput, error)
	CreateInternetGateway(context.Context, *ec2.CreateInternetGatewayInput, ...func(*ec2.Options)) (*ec2.CreateInternetGatewayOutput, error)
	AttachInternetGateway(context.Context, *ec2.AttachInternetGatewayInput, ...func(*ec2.Options)) (*ec2.AttachInternetGatewayOutput, error)
	DescribeRouteTables(context.Context, *ec2.DescribeRouteTablesInput, ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error)
	CreateRouteTable(context.Context, *ec2.CreateRouteTableInput, ...func(*ec2.Options)) (*ec2.CreateRouteTableOutput, error)
	CreateRoute(context.Context, *ec2.CreateRouteInput, ...func(*ec2.Options)) (*ec2.CreateRouteOutput, error)
	AssociateRouteTable(context.Context, *ec2.AssociateRouteTableInput, ...func(*ec2.Options)) (*ec2.AssociateRouteTableOutput, error)

	// Used only to resolve core.InstanceTypeAuto into the cheapest spot
	// instance types at node-group creation.
	DescribeInstanceTypes(context.Context, *ec2.DescribeInstanceTypesInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error)
	DescribeInstanceTypeOfferings(context.Context, *ec2.DescribeInstanceTypeOfferingsInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error)
	DescribeSpotPriceHistory(context.Context, *ec2.DescribeSpotPriceHistoryInput, ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error)

	// The rest are used only by DeleteNetwork, reversing EnsureNetwork.
	DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	DeleteSecurityGroup(context.Context, *ec2.DeleteSecurityGroupInput, ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error)
	DetachInternetGateway(context.Context, *ec2.DetachInternetGatewayInput, ...func(*ec2.Options)) (*ec2.DetachInternetGatewayOutput, error)
	DeleteInternetGateway(context.Context, *ec2.DeleteInternetGatewayInput, ...func(*ec2.Options)) (*ec2.DeleteInternetGatewayOutput, error)
	DeleteSubnet(context.Context, *ec2.DeleteSubnetInput, ...func(*ec2.Options)) (*ec2.DeleteSubnetOutput, error)
	DeleteRouteTable(context.Context, *ec2.DeleteRouteTableInput, ...func(*ec2.Options)) (*ec2.DeleteRouteTableOutput, error)
	DeleteVpc(context.Context, *ec2.DeleteVpcInput, ...func(*ec2.Options)) (*ec2.DeleteVpcOutput, error)
}

// Clients bundles the AWS clients the provisioner uses.
type Clients struct {
	eks eksAPI
	iam iamAPI
	ec2 ec2API
	sts stsPresignAPI

	logger *slog.Logger
}

// Option configures Clients.
type Option func(*Clients)

// WithLogger sets the logger every provisioner built over these Clients logs
// through. Defaults to slog.Default() when not given.
func WithLogger(logger *slog.Logger) Option {
	return func(c *Clients) { c.logger = logger }
}

// NewClients builds real AWS clients for a region.
func NewClients(ctx context.Context, region string, opts ...Option) (*Clients, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	c := &Clients{
		eks:    eks.NewFromConfig(cfg),
		iam:    iam.NewFromConfig(cfg),
		ec2:    ec2.NewFromConfig(cfg),
		sts:    newSTSPresigner(cfg),
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// AWS-managed policies. Attaching these rather than authoring equivalents keeps
// the cluster current as AWS extends what EKS control planes and nodes need.
const (
	policyEKSCluster    = "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"
	policyEKSWorkerNode = "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy"
	policyEKSCNI        = "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy"
	policyECRReadOnly   = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"
	policyEBSCSIDriver  = "arn:aws:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy"
	policyEFSCSIDriver  = "arn:aws:iam::aws:policy/service-role/AmazonEFSCSIDriverPolicy"

	// EKS Auto Mode policies: the cluster role needs four extra managed
	// policies beyond policyEKSCluster, and Auto Mode's self-managed nodes
	// assume a dedicated role with its own policy rather than the
	// policyEKSWorkerNode/policyEKSCNI/policyECRReadOnly trio manually
	// managed node groups use.
	policyEKSComputePolicy       = "arn:aws:iam::aws:policy/AmazonEKSComputePolicy"
	policyEKSBlockStoragePolicy  = "arn:aws:iam::aws:policy/AmazonEKSBlockStoragePolicyV2"
	policyEKSLoadBalancingPolicy = "arn:aws:iam::aws:policy/AmazonEKSLoadBalancingPolicy"
	policyEKSNetworkingPolicy    = "arn:aws:iam::aws:policy/AmazonEKSNetworkingPolicy"

	// The Auto Mode node role has no single dedicated policy; AWS documents
	// attaching this pair instead (docs.aws.amazon.com/eks/latest/userguide/
	// auto-create-node-role.html).
	policyEKSWorkerNodeMinimal = "arn:aws:iam::aws:policy/AmazonEKSWorkerNodeMinimalPolicy"
	policyECRPullOnly          = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryPullOnly"
)

// names derives every AWS resource name from the cluster ID, so a cluster's
// resources are identifiable and a second cluster cannot collide with them.
type names struct {
	spec core.ClusterSpec
}

func (n names) cluster() string     { return n.spec.ID.String() }
func (n names) clusterRole() string { return "kubespin-" + n.spec.ID.String() + "-cluster" }
func (n names) nodeRole() string    { return "kubespin-" + n.spec.ID.String() + "-node" }
func (n names) autoNodeRole() string {
	return "kubespin-" + n.spec.ID.String() + "-auto-node"
}
func (n names) nodeGroup(pool string) string {
	return n.spec.ID.String() + "-" + pool
}

// addonRole names the Pod Identity role of one add-on. IRSA-era clusters
// used the same names, so their roles are still found and cleaned up.
func (n names) addonRole(comp string) string {
	return "kubespin-" + n.spec.ID.String() + "-" + comp
}

func (n names) ebsCSIRole() string      { return n.addonRole("ebs-csi") }
func (n names) efsCSIRole() string      { return n.addonRole("efs-csi") }
func (n names) externalDNSRole() string { return n.addonRole("external-dns") }
func (n names) clusterAutoscalerRole() string {
	return n.addonRole("cluster-autoscaler")
}

func (n names) vpcName() string { return "kubespin-" + n.spec.ID.String() }
func (n names) subnetName(az string) string {
	return "kubespin-" + n.spec.ID.String() + "-subnet-" + az
}
func (n names) igwName() string        { return "kubespin-" + n.spec.ID.String() + "-igw" }
func (n names) routeTableName() string { return "kubespin-" + n.spec.ID.String() + "-rt" }

func tags(spec core.ClusterSpec) map[string]string {
	return map[string]string{
		"ManagedBy":        "kubespin",
		"kubespin/cluster": spec.ID.String(),
		"kubespin/size":    spec.Size.String(),
	}
}
