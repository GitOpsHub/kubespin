package fleetinfra

import (
	"context"
	"net/url"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	apitypes "github.com/aws/aws-sdk-go-v2/service/apigatewayv2/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	logstypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamotypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// fakeAWS is an in-memory stand-in for the six AWS services this package uses.
// It records every call by name so tests can assert not just the end state but
// which calls were made — which is how --dry-run is held to making none.
type fakeAWS struct {
	calls []string

	account string

	table        *dynamotypes.TableDescription
	pitrEnabled  bool
	logGroups    map[string]*int32 // name -> retention days
	roleExists   bool
	rolePolicy   string // URL-encoded, as IAM returns it
	function     *lambdatypes.FunctionConfiguration
	lambdaPolicy string
	apis         []apitypes.Api
	integrations []apitypes.Integration
	routes       []apitypes.Route
	stage        *apigatewayv2.GetStageOutput
	nextID       int
}

func newFakeAWS() *fakeAWS {
	return &fakeAWS{account: testAccount, logGroups: map[string]*int32{}}
}

func (f *fakeAWS) record(name string) { f.calls = append(f.calls, name) }

func (f *fakeAWS) id(prefix string) string {
	f.nextID++
	return prefix + string(rune('a'+f.nextID-1))
}

// called reports whether any of the named calls were made.
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

// mutatingCalls is every call that changes state. A dry run must make none.
var mutatingCalls = []string{
	"CreateTable", "UpdateTable", "UpdateContinuousBackups",
	"CreateLogGroup", "PutRetentionPolicy",
	"CreateRole", "PutRolePolicy",
	"CreateFunction", "UpdateFunctionCode", "UpdateFunctionConfiguration", "AddPermission",
	"CreateApi", "CreateIntegration", "CreateRoute", "CreateStage", "UpdateStage",
}

func (f *fakeAWS) assertNoMutations(t *testing.T) {
	t.Helper()
	for _, call := range f.calls {
		for _, mutator := range mutatingCalls {
			if call == mutator {
				t.Errorf("dry run made mutating call %s", call)
			}
		}
	}
}

func (f *fakeAWS) clients() *Clients {
	return &Clients{sts: f, dynamo: f, logs: f, iam: f, lambda: f, apiGateway: f}
}

// --- STS ---

func (f *fakeAWS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	f.record("GetCallerIdentity")
	return &sts.GetCallerIdentityOutput{Account: aws.String(f.account)}, nil
}

// --- DynamoDB ---

func (f *fakeAWS) DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	f.record("DescribeTable")
	if f.table == nil {
		return nil, &dynamotypes.ResourceNotFoundException{}
	}
	return &dynamodb.DescribeTableOutput{Table: f.table}, nil
}

func (f *fakeAWS) CreateTable(_ context.Context, in *dynamodb.CreateTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	f.record("CreateTable")
	f.table = &dynamotypes.TableDescription{
		TableName:                 in.TableName,
		TableStatus:               dynamotypes.TableStatusActive,
		DeletionProtectionEnabled: in.DeletionProtectionEnabled,
		SSEDescription:            &dynamotypes.SSEDescription{Status: dynamotypes.SSEStatusEnabled},
		GlobalSecondaryIndexes: []dynamotypes.GlobalSecondaryIndexDescription{
			{IndexName: aws.String(gsiName)},
		},
	}
	return &dynamodb.CreateTableOutput{}, nil
}

func (f *fakeAWS) UpdateTable(_ context.Context, in *dynamodb.UpdateTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateTableOutput, error) {
	f.record("UpdateTable")
	if in.DeletionProtectionEnabled != nil {
		f.table.DeletionProtectionEnabled = in.DeletionProtectionEnabled
	}
	if in.SSESpecification != nil {
		f.table.SSEDescription = &dynamotypes.SSEDescription{Status: dynamotypes.SSEStatusEnabled}
	}
	for _, update := range in.GlobalSecondaryIndexUpdates {
		if update.Create != nil {
			f.table.GlobalSecondaryIndexes = append(f.table.GlobalSecondaryIndexes,
				dynamotypes.GlobalSecondaryIndexDescription{IndexName: update.Create.IndexName})
		}
	}
	return &dynamodb.UpdateTableOutput{}, nil
}

func (f *fakeAWS) DescribeContinuousBackups(context.Context, *dynamodb.DescribeContinuousBackupsInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeContinuousBackupsOutput, error) {
	f.record("DescribeContinuousBackups")

	status := dynamotypes.PointInTimeRecoveryStatusDisabled
	if f.pitrEnabled {
		status = dynamotypes.PointInTimeRecoveryStatusEnabled
	}
	return &dynamodb.DescribeContinuousBackupsOutput{
		ContinuousBackupsDescription: &dynamotypes.ContinuousBackupsDescription{
			PointInTimeRecoveryDescription: &dynamotypes.PointInTimeRecoveryDescription{
				PointInTimeRecoveryStatus: status,
			},
		},
	}, nil
}

func (f *fakeAWS) UpdateContinuousBackups(context.Context, *dynamodb.UpdateContinuousBackupsInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateContinuousBackupsOutput, error) {
	f.record("UpdateContinuousBackups")
	f.pitrEnabled = true
	return &dynamodb.UpdateContinuousBackupsOutput{}, nil
}

// --- CloudWatch Logs ---

func (f *fakeAWS) DescribeLogGroups(_ context.Context, in *cloudwatchlogs.DescribeLogGroupsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DescribeLogGroupsOutput, error) {
	f.record("DescribeLogGroups")

	out := &cloudwatchlogs.DescribeLogGroupsOutput{}
	for name, retention := range f.logGroups {
		if name == aws.ToString(in.LogGroupNamePrefix) {
			out.LogGroups = append(out.LogGroups, logstypes.LogGroup{
				LogGroupName:    aws.String(name),
				RetentionInDays: retention,
			})
		}
	}
	return out, nil
}

func (f *fakeAWS) CreateLogGroup(_ context.Context, in *cloudwatchlogs.CreateLogGroupInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogGroupOutput, error) {
	f.record("CreateLogGroup")
	f.logGroups[aws.ToString(in.LogGroupName)] = nil
	return &cloudwatchlogs.CreateLogGroupOutput{}, nil
}

func (f *fakeAWS) PutRetentionPolicy(_ context.Context, in *cloudwatchlogs.PutRetentionPolicyInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutRetentionPolicyOutput, error) {
	f.record("PutRetentionPolicy")
	f.logGroups[aws.ToString(in.LogGroupName)] = in.RetentionInDays
	return &cloudwatchlogs.PutRetentionPolicyOutput{}, nil
}

// --- IAM ---

func (f *fakeAWS) GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	f.record("GetRole")
	if !f.roleExists {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	return &iam.GetRoleOutput{Role: &iamtypes.Role{}}, nil
}

func (f *fakeAWS) CreateRole(context.Context, *iam.CreateRoleInput, ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	f.record("CreateRole")
	f.roleExists = true
	return &iam.CreateRoleOutput{}, nil
}

func (f *fakeAWS) GetRolePolicy(context.Context, *iam.GetRolePolicyInput, ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error) {
	f.record("GetRolePolicy")
	if f.rolePolicy == "" {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	return &iam.GetRolePolicyOutput{PolicyDocument: aws.String(f.rolePolicy)}, nil
}

func (f *fakeAWS) PutRolePolicy(_ context.Context, in *iam.PutRolePolicyInput, _ ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	f.record("PutRolePolicy")
	f.rolePolicy = url.QueryEscape(aws.ToString(in.PolicyDocument))
	return &iam.PutRolePolicyOutput{}, nil
}

// --- Lambda ---

func (f *fakeAWS) GetFunction(context.Context, *lambda.GetFunctionInput, ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error) {
	f.record("GetFunction")
	if f.function == nil {
		return nil, &lambdatypes.ResourceNotFoundException{}
	}
	return &lambda.GetFunctionOutput{Configuration: f.function}, nil
}

func (f *fakeAWS) CreateFunction(_ context.Context, in *lambda.CreateFunctionInput, _ ...func(*lambda.Options)) (*lambda.CreateFunctionOutput, error) {
	f.record("CreateFunction")
	f.function = &lambdatypes.FunctionConfiguration{
		FunctionName: in.FunctionName,
		CodeSha256:   aws.String(codeSHA256(in.Code.ZipFile)),
		Timeout:      in.Timeout,
		MemorySize:   in.MemorySize,
		Environment:  &lambdatypes.EnvironmentResponse{Variables: in.Environment.Variables},
	}
	return &lambda.CreateFunctionOutput{}, nil
}

func (f *fakeAWS) UpdateFunctionCode(_ context.Context, in *lambda.UpdateFunctionCodeInput, _ ...func(*lambda.Options)) (*lambda.UpdateFunctionCodeOutput, error) {
	f.record("UpdateFunctionCode")
	f.function.CodeSha256 = aws.String(codeSHA256(in.ZipFile))
	return &lambda.UpdateFunctionCodeOutput{}, nil
}

func (f *fakeAWS) UpdateFunctionConfiguration(_ context.Context, in *lambda.UpdateFunctionConfigurationInput, _ ...func(*lambda.Options)) (*lambda.UpdateFunctionConfigurationOutput, error) {
	f.record("UpdateFunctionConfiguration")
	f.function.Timeout = in.Timeout
	f.function.MemorySize = in.MemorySize
	f.function.Environment = &lambdatypes.EnvironmentResponse{Variables: in.Environment.Variables}
	return &lambda.UpdateFunctionConfigurationOutput{}, nil
}

func (f *fakeAWS) GetPolicy(context.Context, *lambda.GetPolicyInput, ...func(*lambda.Options)) (*lambda.GetPolicyOutput, error) {
	f.record("GetPolicy")
	if f.lambdaPolicy == "" {
		return nil, &lambdatypes.ResourceNotFoundException{}
	}
	return &lambda.GetPolicyOutput{Policy: aws.String(f.lambdaPolicy)}, nil
}

func (f *fakeAWS) AddPermission(_ context.Context, in *lambda.AddPermissionInput, _ ...func(*lambda.Options)) (*lambda.AddPermissionOutput, error) {
	f.record("AddPermission")
	f.lambdaPolicy = `{"Statement":[{"Sid":"` + aws.ToString(in.StatementId) + `"}]}`
	return &lambda.AddPermissionOutput{}, nil
}

// --- API Gateway v2 ---

func (f *fakeAWS) GetApis(context.Context, *apigatewayv2.GetApisInput, ...func(*apigatewayv2.Options)) (*apigatewayv2.GetApisOutput, error) {
	f.record("GetApis")
	return &apigatewayv2.GetApisOutput{Items: f.apis}, nil
}

func (f *fakeAWS) CreateApi(_ context.Context, in *apigatewayv2.CreateApiInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.CreateApiOutput, error) {
	f.record("CreateApi")

	id := f.id("api")
	endpoint := "https://" + id + ".execute-api.us-east-1.amazonaws.com"
	f.apis = append(f.apis, apitypes.Api{Name: in.Name, ApiId: aws.String(id), ApiEndpoint: aws.String(endpoint)})

	return &apigatewayv2.CreateApiOutput{ApiId: aws.String(id), ApiEndpoint: aws.String(endpoint)}, nil
}

func (f *fakeAWS) GetIntegrations(context.Context, *apigatewayv2.GetIntegrationsInput, ...func(*apigatewayv2.Options)) (*apigatewayv2.GetIntegrationsOutput, error) {
	f.record("GetIntegrations")
	return &apigatewayv2.GetIntegrationsOutput{Items: f.integrations}, nil
}

func (f *fakeAWS) CreateIntegration(_ context.Context, in *apigatewayv2.CreateIntegrationInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.CreateIntegrationOutput, error) {
	f.record("CreateIntegration")

	id := f.id("int")
	f.integrations = append(f.integrations, apitypes.Integration{
		IntegrationId:  aws.String(id),
		IntegrationUri: in.IntegrationUri,
	})
	return &apigatewayv2.CreateIntegrationOutput{IntegrationId: aws.String(id)}, nil
}

func (f *fakeAWS) GetRoutes(context.Context, *apigatewayv2.GetRoutesInput, ...func(*apigatewayv2.Options)) (*apigatewayv2.GetRoutesOutput, error) {
	f.record("GetRoutes")
	return &apigatewayv2.GetRoutesOutput{Items: f.routes}, nil
}

func (f *fakeAWS) CreateRoute(_ context.Context, in *apigatewayv2.CreateRouteInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.CreateRouteOutput, error) {
	f.record("CreateRoute")
	f.routes = append(f.routes, apitypes.Route{RouteKey: in.RouteKey, Target: in.Target})
	return &apigatewayv2.CreateRouteOutput{}, nil
}

func (f *fakeAWS) GetStage(context.Context, *apigatewayv2.GetStageInput, ...func(*apigatewayv2.Options)) (*apigatewayv2.GetStageOutput, error) {
	f.record("GetStage")
	if f.stage == nil {
		return nil, &apitypes.NotFoundException{}
	}
	return f.stage, nil
}

func (f *fakeAWS) CreateStage(_ context.Context, in *apigatewayv2.CreateStageInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.CreateStageOutput, error) {
	f.record("CreateStage")
	f.stage = &apigatewayv2.GetStageOutput{
		StageName:            in.StageName,
		DefaultRouteSettings: in.DefaultRouteSettings,
	}
	return &apigatewayv2.CreateStageOutput{}, nil
}

func (f *fakeAWS) UpdateStage(_ context.Context, in *apigatewayv2.UpdateStageInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.UpdateStageOutput, error) {
	f.record("UpdateStage")
	f.stage.DefaultRouteSettings = in.DefaultRouteSettings
	return &apigatewayv2.UpdateStageOutput{}, nil
}

// --- helpers ---

const testAccount = "123456789012"

func testSpec() Spec {
	return Spec{
		AccountID:     testAccount,
		Region:        "us-east-1",
		RegistryTable: "kubespin-fleet-registry",
		LambdaZip:     []byte("fake-zip-bytes"),
	}.withDefaults()
}
