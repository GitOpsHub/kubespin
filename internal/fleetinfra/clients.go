package fleetinfra

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Each service is reached through an interface listing only the calls this
// package makes. Narrow interfaces are what let the whole converge engine be
// unit-tested without credentials, and they document the exact blast radius of
// the permissions a bootstrap operator needs.

type stsAPI interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

type logsAPI interface {
	DescribeLogGroups(context.Context, *cloudwatchlogs.DescribeLogGroupsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DescribeLogGroupsOutput, error)
	CreateLogGroup(context.Context, *cloudwatchlogs.CreateLogGroupInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogGroupOutput, error)
	PutRetentionPolicy(context.Context, *cloudwatchlogs.PutRetentionPolicyInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutRetentionPolicyOutput, error)
}

type iamAPI interface {
	GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	CreateRole(context.Context, *iam.CreateRoleInput, ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
	GetRolePolicy(context.Context, *iam.GetRolePolicyInput, ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error)
	PutRolePolicy(context.Context, *iam.PutRolePolicyInput, ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error)
}

type lambdaAPI interface {
	GetFunction(context.Context, *lambda.GetFunctionInput, ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error)
	CreateFunction(context.Context, *lambda.CreateFunctionInput, ...func(*lambda.Options)) (*lambda.CreateFunctionOutput, error)
	UpdateFunctionCode(context.Context, *lambda.UpdateFunctionCodeInput, ...func(*lambda.Options)) (*lambda.UpdateFunctionCodeOutput, error)
	UpdateFunctionConfiguration(context.Context, *lambda.UpdateFunctionConfigurationInput, ...func(*lambda.Options)) (*lambda.UpdateFunctionConfigurationOutput, error)
	GetPolicy(context.Context, *lambda.GetPolicyInput, ...func(*lambda.Options)) (*lambda.GetPolicyOutput, error)
	AddPermission(context.Context, *lambda.AddPermissionInput, ...func(*lambda.Options)) (*lambda.AddPermissionOutput, error)
}

type apiGatewayAPI interface {
	GetApis(context.Context, *apigatewayv2.GetApisInput, ...func(*apigatewayv2.Options)) (*apigatewayv2.GetApisOutput, error)
	CreateApi(context.Context, *apigatewayv2.CreateApiInput, ...func(*apigatewayv2.Options)) (*apigatewayv2.CreateApiOutput, error)
	GetIntegrations(context.Context, *apigatewayv2.GetIntegrationsInput, ...func(*apigatewayv2.Options)) (*apigatewayv2.GetIntegrationsOutput, error)
	CreateIntegration(context.Context, *apigatewayv2.CreateIntegrationInput, ...func(*apigatewayv2.Options)) (*apigatewayv2.CreateIntegrationOutput, error)
	GetRoutes(context.Context, *apigatewayv2.GetRoutesInput, ...func(*apigatewayv2.Options)) (*apigatewayv2.GetRoutesOutput, error)
	CreateRoute(context.Context, *apigatewayv2.CreateRouteInput, ...func(*apigatewayv2.Options)) (*apigatewayv2.CreateRouteOutput, error)
	GetStage(context.Context, *apigatewayv2.GetStageInput, ...func(*apigatewayv2.Options)) (*apigatewayv2.GetStageOutput, error)
	CreateStage(context.Context, *apigatewayv2.CreateStageInput, ...func(*apigatewayv2.Options)) (*apigatewayv2.CreateStageOutput, error)
	UpdateStage(context.Context, *apigatewayv2.UpdateStageInput, ...func(*apigatewayv2.Options)) (*apigatewayv2.UpdateStageOutput, error)
}

// Clients bundles the AWS clients the converge steps use.
type Clients struct {
	sts        stsAPI
	logs       logsAPI
	iam        iamAPI
	lambda     lambdaAPI
	apiGateway apiGatewayAPI
}

// NewClients builds real AWS clients from the ambient credential chain.
func NewClients(ctx context.Context, region string) (*Clients, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	return &Clients{
		sts:        sts.NewFromConfig(cfg),
		logs:       cloudwatchlogs.NewFromConfig(cfg),
		iam:        iam.NewFromConfig(cfg),
		lambda:     lambda.NewFromConfig(cfg),
		apiGateway: apigatewayv2.NewFromConfig(cfg),
	}, nil
}

// verifyAccount is the guard that replaces Terraform's allowed_account_ids: it
// refuses to provision shared fleet infrastructure into any account but the one
// configured, which is what keeps it out of a cluster account.
func (c *Clients) verifyAccount(ctx context.Context, want string) error {
	out, err := c.sts.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("verifying caller identity: %w", err)
	}
	got := aws.ToString(out.Account)
	if got != want {
		return fmt.Errorf("%w: credentials belong to %s, expected %s", ErrAccountMismatch, got, want)
	}
	return nil
}
