package fleetinfra

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

const (
	lambdaTimeoutSeconds = 10
	lambdaMemoryMB       = 256

	// permissionStatementID is stable so re-running converge recognises the
	// permission it added last time instead of stacking duplicates.
	permissionStatementID = "AllowInvokeFromIngestionApi"
)

// functionStep provisions the ingestion Lambda itself.
type functionStep struct {
	c    *Clients
	spec Spec

	create     bool
	updateCode bool
	updateConf bool
}

func newFunctionStep(c *Clients, spec Spec) *functionStep {
	return &functionStep{c: c, spec: spec}
}

func (s *functionStep) Name() string { return "ingestion function" }

func (s *functionStep) Plan(ctx context.Context) (Action, error) {
	action := Action{Resource: s.Name()}

	out, err := s.c.lambda.GetFunction(ctx, &lambda.GetFunctionInput{
		FunctionName: aws.String(s.spec.functionName()),
	})
	if err != nil {
		var missing *lambdatypes.ResourceNotFoundException
		if !errors.As(err, &missing) {
			return action, fmt.Errorf("getting function: %w", err)
		}

		s.create = true
		action.Kind = ActionCreate
		action.Details = []string{"provided.al2023 on arm64"}
		return action, nil
	}

	cfg := out.Configuration
	if cfg == nil {
		return action, fmt.Errorf("getting function: empty configuration for %s", s.spec.functionName())
	}

	// The deployed hash against the packaged archive. This comparison is why the
	// zip has to be byte-deterministic.
	if aws.ToString(cfg.CodeSha256) != codeSHA256(s.spec.LambdaZip) {
		s.updateCode = true
		action.Details = append(action.Details, "handler code changed")
	}
	if envValue(cfg.Environment, "REGISTRY_TABLE") != s.spec.RegistryTable {
		s.updateConf = true
		action.Details = append(action.Details, "REGISTRY_TABLE environment variable")
	}
	if aws.ToInt32(cfg.Timeout) != lambdaTimeoutSeconds || aws.ToInt32(cfg.MemorySize) != lambdaMemoryMB {
		s.updateConf = true
		action.Details = append(action.Details, "timeout or memory size")
	}

	if len(action.Details) > 0 {
		action.Kind = ActionUpdate
	}
	return action, nil
}

func (s *functionStep) Apply(ctx context.Context, _ Action) error {
	if s.create {
		_, err := s.c.lambda.CreateFunction(ctx, &lambda.CreateFunctionInput{
			FunctionName:  aws.String(s.spec.functionName()),
			Description:   aws.String("Verifies per-cloud workload identity signatures and writes cluster status to the Fleet Registry"),
			Role:          aws.String(s.spec.roleARN()),
			Handler:       aws.String(handlerName),
			Runtime:       lambdatypes.RuntimeProvidedal2023,
			Architectures: []lambdatypes.Architecture{lambdatypes.ArchitectureArm64},
			Code:          &lambdatypes.FunctionCode{ZipFile: s.spec.LambdaZip},
			Timeout:       aws.Int32(lambdaTimeoutSeconds),
			MemorySize:    aws.Int32(lambdaMemoryMB),
			Environment:   &lambdatypes.Environment{Variables: s.environment()},
		})
		if err != nil {
			return fmt.Errorf("creating function: %w", err)
		}
		return nil
	}

	if s.updateCode {
		_, err := s.c.lambda.UpdateFunctionCode(ctx, &lambda.UpdateFunctionCodeInput{
			FunctionName: aws.String(s.spec.functionName()),
			ZipFile:      s.spec.LambdaZip,
		})
		if err != nil {
			return fmt.Errorf("updating function code: %w", err)
		}
	}
	if s.updateConf {
		_, err := s.c.lambda.UpdateFunctionConfiguration(ctx, &lambda.UpdateFunctionConfigurationInput{
			FunctionName: aws.String(s.spec.functionName()),
			Timeout:      aws.Int32(lambdaTimeoutSeconds),
			MemorySize:   aws.Int32(lambdaMemoryMB),
			Environment:  &lambdatypes.Environment{Variables: s.environment()},
		})
		if err != nil {
			return fmt.Errorf("updating function configuration: %w", err)
		}
	}
	return nil
}

func (s *functionStep) environment() map[string]string {
	return map[string]string{"REGISTRY_TABLE": s.spec.RegistryTable}
}

func envValue(env *lambdatypes.EnvironmentResponse, key string) string {
	if env == nil {
		return ""
	}
	return env.Variables[key]
}

// permissionStep lets API Gateway invoke the function.
//
// It holds the API step rather than the API id, because the id only exists once
// that step has run — and on a dry run against an empty account, it never will.
type permissionStep struct {
	c    *Clients
	spec Spec
	api  *apiStep

	add bool
}

func newPermissionStep(c *Clients, spec Spec, api *apiStep) *permissionStep {
	return &permissionStep{c: c, spec: spec, api: api}
}

func (s *permissionStep) Name() string { return "invoke permission" }

func (s *permissionStep) Plan(ctx context.Context) (Action, error) {
	action := Action{Resource: s.Name()}

	out, err := s.c.lambda.GetPolicy(ctx, &lambda.GetPolicyInput{
		FunctionName: aws.String(s.spec.functionName()),
	})
	if err != nil {
		var missing *lambdatypes.ResourceNotFoundException
		if !errors.As(err, &missing) {
			return action, fmt.Errorf("getting function policy: %w", err)
		}
		// No policy at all, or no function yet — either way the permission has
		// to be added once the function exists.
		s.add = true
		action.Kind = ActionCreate
		action.Details = []string{"allow apigateway.amazonaws.com to invoke"}
		return action, nil
	}

	if !strings.Contains(aws.ToString(out.Policy), permissionStatementID) {
		s.add = true
		action.Kind = ActionCreate
		action.Details = []string{"allow apigateway.amazonaws.com to invoke"}
	}
	return action, nil
}

func (s *permissionStep) Apply(ctx context.Context, _ Action) error {
	if !s.add {
		return nil
	}

	// Scoped to this one API: any other caller, including another API in the
	// same account, is refused.
	sourceARN, err := s.api.executeARN()
	if err != nil {
		return err
	}

	_, err = s.c.lambda.AddPermission(ctx, &lambda.AddPermissionInput{
		FunctionName: aws.String(s.spec.functionName()),
		StatementId:  aws.String(permissionStatementID),
		Action:       aws.String("lambda:InvokeFunction"),
		Principal:    aws.String("apigateway.amazonaws.com"),
		SourceArn:    aws.String(sourceARN),
	})
	if err != nil {
		// A concurrent or interrupted earlier run may have added it already;
		// that is convergence succeeding, not failing.
		var conflict *lambdatypes.ResourceConflictException
		if errors.As(err, &conflict) {
			return nil
		}
		return fmt.Errorf("adding invoke permission: %w", err)
	}
	return nil
}
