package fleetinfra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	apitypes "github.com/aws/aws-sdk-go-v2/service/apigatewayv2/types"
)

const stageName = "$default"

// apiStep provisions the Central Ingestion API: the HTTP API, its Lambda proxy
// integration, the single status route, and the auto-deploying default stage.
//
// These four are one step because they are meaningless apart — an API with no
// route is not a partially working ingestion endpoint, it is a broken one.
type apiStep struct {
	c    *Clients
	spec Spec

	// Resolved during Plan, or filled in by Apply when it creates them.
	apiID         string
	apiEndpoint   string
	integrationID string

	createAPI         bool
	createIntegration bool
	createRoute       bool
	createStage       bool
	updateStage       bool
}

func newAPIStep(c *Clients, spec Spec) *apiStep {
	return &apiStep{c: c, spec: spec}
}

func (s *apiStep) Name() string { return "ingestion API" }

// endpoint is the base URL clusters push to, empty until the API exists.
func (s *apiStep) endpoint() string {
	if s.apiEndpoint == "" {
		return ""
	}
	return s.apiEndpoint + statusRoutePath
}

// executeARN scopes the Lambda invoke permission to this API.
func (s *apiStep) executeARN() (string, error) {
	if s.apiID == "" {
		return "", errors.New("ingestion API id is not known yet")
	}
	return fmt.Sprintf("arn:aws:execute-api:%s:%s:%s/*/*", s.spec.Region, s.spec.AccountID, s.apiID), nil
}

func (s *apiStep) Plan(ctx context.Context) (Action, error) {
	action := Action{Resource: s.Name()}

	api, err := s.findAPI(ctx)
	if err != nil {
		return action, err
	}
	if api == nil {
		s.createAPI, s.createIntegration, s.createRoute, s.createStage = true, true, true, true
		action.Kind = ActionCreate
		action.Details = []string{"HTTP API", "lambda integration", StatusRouteKey, stageName + " stage"}
		return action, nil
	}

	s.apiID = aws.ToString(api.ApiId)
	s.apiEndpoint = aws.ToString(api.ApiEndpoint)

	if err := s.planIntegration(ctx, &action); err != nil {
		return action, err
	}
	if err := s.planRoute(ctx, &action); err != nil {
		return action, err
	}
	if err := s.planStage(ctx, &action); err != nil {
		return action, err
	}

	if len(action.Details) > 0 {
		action.Kind = ActionUpdate
	}
	return action, nil
}

func (s *apiStep) findAPI(ctx context.Context) (*apitypes.Api, error) {
	// HTTP APIs are not addressable by name, so the whole account's APIs are
	// listed and matched. GetApis pages at 25 by default; ask for the maximum.
	out, err := s.c.apiGateway.GetApis(ctx, &apigatewayv2.GetApisInput{MaxResults: aws.String("500")})
	if err != nil {
		return nil, fmt.Errorf("listing APIs: %w", err)
	}

	for i, api := range out.Items {
		if aws.ToString(api.Name) == s.spec.apiName() {
			return &out.Items[i], nil
		}
	}
	return nil, nil
}

func (s *apiStep) planIntegration(ctx context.Context, action *Action) error {
	out, err := s.c.apiGateway.GetIntegrations(ctx, &apigatewayv2.GetIntegrationsInput{
		ApiId: aws.String(s.apiID),
	})
	if err != nil {
		return fmt.Errorf("listing integrations: %w", err)
	}

	for _, integration := range out.Items {
		if aws.ToString(integration.IntegrationUri) == s.spec.invokeARN() {
			s.integrationID = aws.ToString(integration.IntegrationId)
			return nil
		}
	}

	s.createIntegration = true
	action.Details = append(action.Details, "create lambda integration")
	return nil
}

func (s *apiStep) planRoute(ctx context.Context, action *Action) error {
	out, err := s.c.apiGateway.GetRoutes(ctx, &apigatewayv2.GetRoutesInput{ApiId: aws.String(s.apiID)})
	if err != nil {
		return fmt.Errorf("listing routes: %w", err)
	}

	for _, route := range out.Items {
		if aws.ToString(route.RouteKey) == StatusRouteKey {
			return nil
		}
	}

	s.createRoute = true
	action.Details = append(action.Details, "create route "+StatusRouteKey)
	return nil
}

func (s *apiStep) planStage(ctx context.Context, action *Action) error {
	out, err := s.c.apiGateway.GetStage(ctx, &apigatewayv2.GetStageInput{
		ApiId:     aws.String(s.apiID),
		StageName: aws.String(stageName),
	})
	if err != nil {
		var missing *apitypes.NotFoundException
		if !errors.As(err, &missing) {
			return fmt.Errorf("getting stage: %w", err)
		}
		s.createStage = true
		action.Details = append(action.Details, "create "+stageName+" stage")
		return nil
	}

	settings := out.DefaultRouteSettings
	if settings == nil ||
		aws.ToInt32(settings.ThrottlingBurstLimit) != s.spec.ThrottleBurst ||
		aws.ToFloat64(settings.ThrottlingRateLimit) != s.spec.ThrottleRate {
		s.updateStage = true
		action.Details = append(action.Details, "adjust throttle limits")
	}
	return nil
}

func (s *apiStep) Apply(ctx context.Context, _ Action) error {
	if s.createAPI {
		out, err := s.c.apiGateway.CreateApi(ctx, &apigatewayv2.CreateApiInput{
			Name:         aws.String(s.spec.apiName()),
			ProtocolType: apitypes.ProtocolTypeHttp,
			Description:  aws.String("kubespin Central Ingestion API for fleet-status-reporter"),
		})
		if err != nil {
			return fmt.Errorf("creating API: %w", err)
		}
		s.apiID = aws.ToString(out.ApiId)
		s.apiEndpoint = aws.ToString(out.ApiEndpoint)
	}

	if s.createIntegration {
		out, err := s.c.apiGateway.CreateIntegration(ctx, &apigatewayv2.CreateIntegrationInput{
			ApiId:                aws.String(s.apiID),
			IntegrationType:      apitypes.IntegrationTypeAwsProxy,
			IntegrationUri:       aws.String(s.spec.invokeARN()),
			PayloadFormatVersion: aws.String("2.0"),
		})
		if err != nil {
			return fmt.Errorf("creating integration: %w", err)
		}
		s.integrationID = aws.ToString(out.IntegrationId)
	}

	if s.createRoute {
		_, err := s.c.apiGateway.CreateRoute(ctx, &apigatewayv2.CreateRouteInput{
			ApiId:    aws.String(s.apiID),
			RouteKey: aws.String(StatusRouteKey),
			Target:   aws.String("integrations/" + s.integrationID),
			// The caller authenticates with a cloud-native workload identity
			// token verified inside the handler. Three clouds mean three
			// issuers, so this cannot be a single-issuer JWT authorizer here.
			AuthorizationType: apitypes.AuthorizationTypeNone,
		})
		if err != nil {
			return fmt.Errorf("creating route: %w", err)
		}
	}

	if s.createStage {
		_, err := s.c.apiGateway.CreateStage(ctx, &apigatewayv2.CreateStageInput{
			ApiId:                aws.String(s.apiID),
			StageName:            aws.String(stageName),
			AutoDeploy:           aws.Bool(true),
			DefaultRouteSettings: s.routeSettings(),
			AccessLogSettings:    s.accessLogSettings(),
		})
		if err != nil {
			return fmt.Errorf("creating stage: %w", err)
		}
	} else if s.updateStage {
		_, err := s.c.apiGateway.UpdateStage(ctx, &apigatewayv2.UpdateStageInput{
			ApiId:                aws.String(s.apiID),
			StageName:            aws.String(stageName),
			DefaultRouteSettings: s.routeSettings(),
		})
		if err != nil {
			return fmt.Errorf("updating stage: %w", err)
		}
	}

	return nil
}

func (s *apiStep) routeSettings() *apitypes.RouteSettings {
	return &apitypes.RouteSettings{
		ThrottlingBurstLimit: aws.Int32(s.spec.ThrottleBurst),
		ThrottlingRateLimit:  aws.Float64(s.spec.ThrottleRate),
	}
}

func (s *apiStep) accessLogSettings() *apitypes.AccessLogSettings {
	format, err := json.Marshal(map[string]string{
		"requestId":      "$context.requestId",
		"routeKey":       "$context.routeKey",
		"status":         "$context.status",
		"responseLength": "$context.responseLength",
		"requestTime":    "$context.requestTime",
		"integrationErr": "$context.integrationErrorMessage",
	})
	if err != nil {
		return nil
	}

	return &apitypes.AccessLogSettings{
		DestinationArn: aws.String(s.spec.apiLogGroupARN()),
		Format:         aws.String(string(format)),
	}
}
