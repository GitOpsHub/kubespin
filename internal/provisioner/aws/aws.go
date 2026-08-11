// Package aws provisions EKS clusters and IRSA identities.
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
}

// iamAPI covers both the service roles EKS needs and the IRSA role.
type iamAPI interface {
	GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	CreateRole(context.Context, *iam.CreateRoleInput, ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
	DeleteRole(context.Context, *iam.DeleteRoleInput, ...func(*iam.Options)) (*iam.DeleteRoleOutput, error)
	UpdateAssumeRolePolicy(context.Context, *iam.UpdateAssumeRolePolicyInput, ...func(*iam.Options)) (*iam.UpdateAssumeRolePolicyOutput, error)
	AttachRolePolicy(context.Context, *iam.AttachRolePolicyInput, ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error)
	ListAttachedRolePolicies(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
	DetachRolePolicy(context.Context, *iam.DetachRolePolicyInput, ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error)
	ListOpenIDConnectProviders(context.Context, *iam.ListOpenIDConnectProvidersInput, ...func(*iam.Options)) (*iam.ListOpenIDConnectProvidersOutput, error)
	GetOpenIDConnectProvider(context.Context, *iam.GetOpenIDConnectProviderInput, ...func(*iam.Options)) (*iam.GetOpenIDConnectProviderOutput, error)
	CreateOpenIDConnectProvider(context.Context, *iam.CreateOpenIDConnectProviderInput, ...func(*iam.Options)) (*iam.CreateOpenIDConnectProviderOutput, error)
}

// ec2API covers the status reporter's egress rule and, when spec.Subnets is
// empty, the VPC/subnets/Internet Gateway/route table EnsureNetwork creates.
type ec2API interface {
	DescribeSecurityGroupRules(context.Context, *ec2.DescribeSecurityGroupRulesInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupRulesOutput, error)
	AuthorizeSecurityGroupEgress(context.Context, *ec2.AuthorizeSecurityGroupEgressInput, ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupEgressOutput, error)

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
	policyEKSCluster        = "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"
	policyEKSWorkerNode     = "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy"
	policyEKSCNI            = "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy"
	policyECRReadOnly       = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"
	eksOIDCThumbprint       = "9e99a48a9960b14926bb7f3b02e22da2b0ab7280"
	eksOIDCClientIDAudience = "sts.amazonaws.com"
)

// names derives every AWS resource name from the cluster ID, so a cluster's
// resources are identifiable and a second cluster cannot collide with them.
type names struct {
	spec core.ClusterSpec
}

func (n names) cluster() string     { return n.spec.ID.String() }
func (n names) clusterRole() string { return "kubespin-" + n.spec.ID.String() + "-cluster" }
func (n names) nodeRole() string    { return "kubespin-" + n.spec.ID.String() + "-node" }
func (n names) nodeGroup(pool string) string {
	return n.spec.ID.String() + "-" + pool
}

func (n names) irsaRole(comp string) string {
	return "kubespin-" + n.spec.ID.String() + "-" + comp
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
		"kubespin/profile": spec.Profile.String(),
	}
}
