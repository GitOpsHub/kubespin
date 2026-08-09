package aws

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// fakeAWS stands in for EKS, IAM, and EC2, recording every call by name so
// tests can assert which calls were made — which is how "reconcile changed
// nothing" is held to making no mutating calls at all.
type fakeAWS struct {
	calls []string

	cluster    *ekstypes.Cluster
	nodeGroups map[string]*ekstypes.Nodegroup
	roles      map[string]string // name -> arn
	rolePolicy map[string]string // name -> assume role policy document
	attached   map[string][]string
	oidc       map[string]string // arn -> url host
	sgRules    []ec2types.SecurityGroupRule
}

func newFakeAWS() *fakeAWS {
	return &fakeAWS{
		nodeGroups: map[string]*ekstypes.Nodegroup{},
		roles:      map[string]string{},
		rolePolicy: map[string]string{},
		attached:   map[string][]string{},
		oidc:       map[string]string{},
	}
}

func (f *fakeAWS) record(name string) { f.calls = append(f.calls, name) }

func (f *fakeAWS) called(names ...string) bool {
	for _, call := range f.calls {
		for _, name := range names {
			if call == name {
				return true
			}
		}
	}
	return false
}

// mutatingCalls is every call that changes cloud state.
var mutatingCalls = []string{
	"CreateCluster", "UpdateClusterConfig", "DeleteCluster",
	"CreateNodegroup", "UpdateNodegroupConfig", "DeleteNodegroup",
	"CreateRole", "DeleteRole", "AttachRolePolicy", "DetachRolePolicy",
	"UpdateAssumeRolePolicy", "CreateOpenIDConnectProvider",
	"AuthorizeSecurityGroupEgress",
}

func (f *fakeAWS) assertNoMutations(t *testing.T) {
	t.Helper()
	for _, call := range f.calls {
		for _, mutator := range mutatingCalls {
			if call == mutator {
				t.Errorf("expected no cloud changes, but %s was called", call)
			}
		}
	}
}

func (f *fakeAWS) clients() *Clients { return &Clients{eks: f, iam: f, ec2: f} }

// --- EKS ---

func (f *fakeAWS) DescribeCluster(context.Context, *eks.DescribeClusterInput, ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	f.record("DescribeCluster")
	if f.cluster == nil {
		return nil, &ekstypes.ResourceNotFoundException{}
	}
	return &eks.DescribeClusterOutput{Cluster: f.cluster}, nil
}

func (f *fakeAWS) CreateCluster(_ context.Context, in *eks.CreateClusterInput, _ ...func(*eks.Options)) (*eks.CreateClusterOutput, error) {
	f.record("CreateCluster")
	f.cluster = &ekstypes.Cluster{
		Name:    in.Name,
		Status:  ekstypes.ClusterStatusCreating,
		Version: in.Version,
		ResourcesVpcConfig: &ekstypes.VpcConfigResponse{
			SubnetIds:             in.ResourcesVpcConfig.SubnetIds,
			EndpointPublicAccess:  aws.ToBool(in.ResourcesVpcConfig.EndpointPublicAccess),
			EndpointPrivateAccess: aws.ToBool(in.ResourcesVpcConfig.EndpointPrivateAccess),
			PublicAccessCidrs:     in.ResourcesVpcConfig.PublicAccessCidrs,
		},
	}
	return &eks.CreateClusterOutput{}, nil
}

func (f *fakeAWS) UpdateClusterConfig(_ context.Context, in *eks.UpdateClusterConfigInput, _ ...func(*eks.Options)) (*eks.UpdateClusterConfigOutput, error) {
	f.record("UpdateClusterConfig")
	f.cluster.ResourcesVpcConfig.EndpointPublicAccess = aws.ToBool(in.ResourcesVpcConfig.EndpointPublicAccess)
	f.cluster.ResourcesVpcConfig.PublicAccessCidrs = in.ResourcesVpcConfig.PublicAccessCidrs
	return &eks.UpdateClusterConfigOutput{}, nil
}

func (f *fakeAWS) DeleteCluster(context.Context, *eks.DeleteClusterInput, ...func(*eks.Options)) (*eks.DeleteClusterOutput, error) {
	f.record("DeleteCluster")
	if f.cluster == nil {
		return nil, &ekstypes.ResourceNotFoundException{}
	}
	f.cluster = nil
	return &eks.DeleteClusterOutput{}, nil
}

func (f *fakeAWS) ListNodegroups(context.Context, *eks.ListNodegroupsInput, ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error) {
	f.record("ListNodegroups")
	if f.cluster == nil {
		return nil, &ekstypes.ResourceNotFoundException{}
	}

	names := make([]string, 0, len(f.nodeGroups))
	for name := range f.nodeGroups {
		names = append(names, name)
	}
	return &eks.ListNodegroupsOutput{Nodegroups: names}, nil
}

func (f *fakeAWS) DescribeNodegroup(_ context.Context, in *eks.DescribeNodegroupInput, _ ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error) {
	f.record("DescribeNodegroup")
	ng, ok := f.nodeGroups[aws.ToString(in.NodegroupName)]
	if !ok {
		return nil, &ekstypes.ResourceNotFoundException{}
	}
	return &eks.DescribeNodegroupOutput{Nodegroup: ng}, nil
}

func (f *fakeAWS) CreateNodegroup(_ context.Context, in *eks.CreateNodegroupInput, _ ...func(*eks.Options)) (*eks.CreateNodegroupOutput, error) {
	f.record("CreateNodegroup")
	f.nodeGroups[aws.ToString(in.NodegroupName)] = &ekstypes.Nodegroup{
		NodegroupName: in.NodegroupName,
		InstanceTypes: in.InstanceTypes,
		ScalingConfig: in.ScalingConfig,
		Labels:        in.Labels,
	}
	return &eks.CreateNodegroupOutput{}, nil
}

func (f *fakeAWS) UpdateNodegroupConfig(_ context.Context, in *eks.UpdateNodegroupConfigInput, _ ...func(*eks.Options)) (*eks.UpdateNodegroupConfigOutput, error) {
	f.record("UpdateNodegroupConfig")
	f.nodeGroups[aws.ToString(in.NodegroupName)].ScalingConfig = in.ScalingConfig
	return &eks.UpdateNodegroupConfigOutput{}, nil
}

func (f *fakeAWS) DeleteNodegroup(_ context.Context, in *eks.DeleteNodegroupInput, _ ...func(*eks.Options)) (*eks.DeleteNodegroupOutput, error) {
	f.record("DeleteNodegroup")
	delete(f.nodeGroups, aws.ToString(in.NodegroupName))
	return &eks.DeleteNodegroupOutput{}, nil
}

// --- IAM ---

func (f *fakeAWS) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	f.record("GetRole")
	arn, ok := f.roles[aws.ToString(in.RoleName)]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	return &iam.GetRoleOutput{Role: &iamtypes.Role{RoleName: in.RoleName, Arn: aws.String(arn)}}, nil
}

func (f *fakeAWS) CreateRole(_ context.Context, in *iam.CreateRoleInput, _ ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	f.record("CreateRole")

	name := aws.ToString(in.RoleName)
	arn := "arn:aws:iam::123456789012:role/" + name
	f.roles[name] = arn
	f.rolePolicy[name] = aws.ToString(in.AssumeRolePolicyDocument)

	return &iam.CreateRoleOutput{Role: &iamtypes.Role{RoleName: in.RoleName, Arn: aws.String(arn)}}, nil
}

func (f *fakeAWS) DeleteRole(_ context.Context, in *iam.DeleteRoleInput, _ ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	f.record("DeleteRole")

	name := aws.ToString(in.RoleName)
	if _, ok := f.roles[name]; !ok {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	delete(f.roles, name)
	return &iam.DeleteRoleOutput{}, nil
}

func (f *fakeAWS) UpdateAssumeRolePolicy(_ context.Context, in *iam.UpdateAssumeRolePolicyInput, _ ...func(*iam.Options)) (*iam.UpdateAssumeRolePolicyOutput, error) {
	f.record("UpdateAssumeRolePolicy")
	f.rolePolicy[aws.ToString(in.RoleName)] = aws.ToString(in.PolicyDocument)
	return &iam.UpdateAssumeRolePolicyOutput{}, nil
}

func (f *fakeAWS) AttachRolePolicy(_ context.Context, in *iam.AttachRolePolicyInput, _ ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error) {
	f.record("AttachRolePolicy")

	name := aws.ToString(in.RoleName)
	f.attached[name] = append(f.attached[name], aws.ToString(in.PolicyArn))
	return &iam.AttachRolePolicyOutput{}, nil
}

func (f *fakeAWS) ListAttachedRolePolicies(_ context.Context, in *iam.ListAttachedRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	f.record("ListAttachedRolePolicies")

	name := aws.ToString(in.RoleName)
	if _, ok := f.roles[name]; !ok {
		return nil, &iamtypes.NoSuchEntityException{}
	}

	var out []iamtypes.AttachedPolicy
	for _, arn := range f.attached[name] {
		out = append(out, iamtypes.AttachedPolicy{PolicyArn: aws.String(arn)})
	}
	return &iam.ListAttachedRolePoliciesOutput{AttachedPolicies: out}, nil
}

func (f *fakeAWS) DetachRolePolicy(_ context.Context, in *iam.DetachRolePolicyInput, _ ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	f.record("DetachRolePolicy")

	name := aws.ToString(in.RoleName)
	remaining := f.attached[name][:0]
	for _, arn := range f.attached[name] {
		if arn != aws.ToString(in.PolicyArn) {
			remaining = append(remaining, arn)
		}
	}
	f.attached[name] = remaining
	return &iam.DetachRolePolicyOutput{}, nil
}

func (f *fakeAWS) ListOpenIDConnectProviders(context.Context, *iam.ListOpenIDConnectProvidersInput, ...func(*iam.Options)) (*iam.ListOpenIDConnectProvidersOutput, error) {
	f.record("ListOpenIDConnectProviders")

	var out []iamtypes.OpenIDConnectProviderListEntry
	for arn := range f.oidc {
		out = append(out, iamtypes.OpenIDConnectProviderListEntry{Arn: aws.String(arn)})
	}
	return &iam.ListOpenIDConnectProvidersOutput{OpenIDConnectProviderList: out}, nil
}

func (f *fakeAWS) GetOpenIDConnectProvider(_ context.Context, in *iam.GetOpenIDConnectProviderInput, _ ...func(*iam.Options)) (*iam.GetOpenIDConnectProviderOutput, error) {
	f.record("GetOpenIDConnectProvider")

	host, ok := f.oidc[aws.ToString(in.OpenIDConnectProviderArn)]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	return &iam.GetOpenIDConnectProviderOutput{Url: aws.String(host)}, nil
}

func (f *fakeAWS) CreateOpenIDConnectProvider(_ context.Context, in *iam.CreateOpenIDConnectProviderInput, _ ...func(*iam.Options)) (*iam.CreateOpenIDConnectProviderOutput, error) {
	f.record("CreateOpenIDConnectProvider")

	host := strings.TrimPrefix(aws.ToString(in.Url), "https://")
	arn := "arn:aws:iam::123456789012:oidc-provider/" + host
	f.oidc[arn] = host

	return &iam.CreateOpenIDConnectProviderOutput{OpenIDConnectProviderArn: aws.String(arn)}, nil
}

// --- EC2 ---

func (f *fakeAWS) DescribeSecurityGroupRules(context.Context, *ec2.DescribeSecurityGroupRulesInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupRulesOutput, error) {
	f.record("DescribeSecurityGroupRules")
	return &ec2.DescribeSecurityGroupRulesOutput{SecurityGroupRules: f.sgRules}, nil
}

func (f *fakeAWS) AuthorizeSecurityGroupEgress(_ context.Context, in *ec2.AuthorizeSecurityGroupEgressInput, _ ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupEgressOutput, error) {
	f.record("AuthorizeSecurityGroupEgress")

	for _, perm := range in.IpPermissions {
		for _, r := range perm.IpRanges {
			f.sgRules = append(f.sgRules, ec2types.SecurityGroupRule{
				IsEgress:   aws.Bool(true),
				IpProtocol: perm.IpProtocol,
				FromPort:   perm.FromPort,
				ToPort:     perm.ToPort,
				CidrIpv4:   r.CidrIp,
			})
		}
	}
	return &ec2.AuthorizeSecurityGroupEgressOutput{}, nil
}

// --- helpers ---

const testIssuer = "https://oidc.eks.us-east-1.amazonaws.com/id/EXAMPLED539D4633E53DE1B716D3041E"

func testSpec() core.ClusterSpec {
	return core.ClusterSpec{
		ID:       "team-payments-prod",
		Provider: core.ProviderAWS,
		Region:   "us-east-1",
		Access:   core.AccessPrivate,
		Subnets:  []string{"subnet-aaa", "subnet-bbb"},
		NodePools: []core.NodePool{{
			Name: "default", InstanceType: "m6i.large", MinSize: 1, MaxSize: 5, DesiredSize: 3,
		}},
		Profile: core.ProfileRef{Name: "tier-small", Version: "1.0.0"},
	}
}

// activeCluster puts the fake into the state a successfully created cluster
// leaves behind.
func (f *fakeAWS) activeCluster(spec core.ClusterSpec) {
	f.cluster = &ekstypes.Cluster{
		Name:     aws.String(spec.ID.String()),
		Status:   ekstypes.ClusterStatusActive,
		Endpoint: aws.String("https://example.eks.amazonaws.com"),
		Identity: &ekstypes.Identity{Oidc: &ekstypes.OIDC{Issuer: aws.String(testIssuer)}},
		ResourcesVpcConfig: &ekstypes.VpcConfigResponse{
			SubnetIds:              spec.Subnets,
			EndpointPublicAccess:   spec.Access == core.AccessPublic,
			EndpointPrivateAccess:  true,
			ClusterSecurityGroupId: aws.String("sg-cluster"),
		},
	}
}

// withNodePool registers a node group matching the given pool.
func (f *fakeAWS) withNodePool(spec core.ClusterSpec, pool core.NodePool) {
	f.nodeGroups[names{spec}.nodeGroup(pool.Name)] = &ekstypes.Nodegroup{
		NodegroupName: aws.String(names{spec}.nodeGroup(pool.Name)),
		InstanceTypes: []string{pool.InstanceType},
		ScalingConfig: &ekstypes.NodegroupScalingConfig{
			MinSize:     aws.Int32(pool.MinSize),
			MaxSize:     aws.Int32(pool.MaxSize),
			DesiredSize: aws.Int32(pool.DesiredSize),
		},
	}
}

// trustPolicy returns a role's decoded assume-role policy document.
func (f *fakeAWS) trustPolicy(t *testing.T, role string) map[string]any {
	t.Helper()

	raw, ok := f.rolePolicy[role]
	if !ok {
		t.Fatalf("no trust policy recorded for role %s", role)
	}
	if decoded, err := url.QueryUnescape(raw); err == nil {
		raw = decoded
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("parsing trust policy for %s: %v", role, err)
	}
	return doc
}
