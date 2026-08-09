package fleetinfra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

const inlinePolicyName = "ingestion"

// roleStep provisions the ingestion Lambda's execution role.
//
// The inline policy is deliberately tiny: the ingestion path only ever updates
// existing cluster records, so it gets no CreateTable, no Scan, no Delete, and
// no access to any table but the registry.
type roleStep struct {
	c    *Clients
	spec Spec

	create    bool
	fixPolicy bool
}

func newRoleStep(c *Clients, spec Spec) *roleStep {
	return &roleStep{c: c, spec: spec}
}

func (s *roleStep) Name() string { return "ingestion role" }

func (s *roleStep) Plan(ctx context.Context) (Action, error) {
	action := Action{Resource: s.Name()}

	_, err := s.c.iam.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(s.spec.roleName())})
	if err != nil {
		var missing *iamtypes.NoSuchEntityException
		if !errors.As(err, &missing) {
			return action, fmt.Errorf("getting role: %w", err)
		}

		s.create, s.fixPolicy = true, true
		action.Kind = ActionCreate
		action.Details = []string{"lambda execution role", "least-privilege inline policy"}
		return action, nil
	}

	current, err := s.currentPolicy(ctx)
	if err != nil {
		return action, err
	}
	if !policyEqual(current, s.desiredPolicy()) {
		s.fixPolicy = true
		action.Kind = ActionUpdate
		action.Details = []string{"inline policy differs from desired"}
	}
	return action, nil
}

// currentPolicy returns the attached inline policy, or nil when absent.
func (s *roleStep) currentPolicy(ctx context.Context) (map[string]any, error) {
	out, err := s.c.iam.GetRolePolicy(ctx, &iam.GetRolePolicyInput{
		RoleName:   aws.String(s.spec.roleName()),
		PolicyName: aws.String(inlinePolicyName),
	})
	if err != nil {
		var missing *iamtypes.NoSuchEntityException
		if errors.As(err, &missing) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting role policy: %w", err)
	}

	// IAM returns the document URL-encoded.
	decoded, err := url.QueryUnescape(aws.ToString(out.PolicyDocument))
	if err != nil {
		return nil, fmt.Errorf("decoding role policy: %w", err)
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(decoded), &doc); err != nil {
		return nil, fmt.Errorf("parsing role policy: %w", err)
	}
	return doc, nil
}

func (s *roleStep) Apply(ctx context.Context, _ Action) error {
	if s.create {
		doc, err := json.Marshal(assumeRolePolicy())
		if err != nil {
			return fmt.Errorf("rendering trust policy: %w", err)
		}

		_, err = s.c.iam.CreateRole(ctx, &iam.CreateRoleInput{
			RoleName:                 aws.String(s.spec.roleName()),
			AssumeRolePolicyDocument: aws.String(string(doc)),
			Description:              aws.String("kubespin Central Ingestion API execution role"),
		})
		if err != nil {
			return fmt.Errorf("creating role: %w", err)
		}
	}

	if s.fixPolicy {
		doc, err := json.Marshal(s.desiredPolicy())
		if err != nil {
			return fmt.Errorf("rendering inline policy: %w", err)
		}

		_, err = s.c.iam.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
			RoleName:       aws.String(s.spec.roleName()),
			PolicyName:     aws.String(inlinePolicyName),
			PolicyDocument: aws.String(string(doc)),
		})
		if err != nil {
			return fmt.Errorf("putting inline policy: %w", err)
		}
	}
	return nil
}

func assumeRolePolicy() map[string]any {
	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{
			map[string]any{
				"Effect":    "Allow",
				"Action":    "sts:AssumeRole",
				"Principal": map[string]any{"Service": "lambda.amazonaws.com"},
			},
		},
	}
}

func (s *roleStep) desiredPolicy() map[string]any {
	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{
			map[string]any{
				"Sid":      "WriteClusterStatus",
				"Effect":   "Allow",
				"Action":   []any{"dynamodb:GetItem", "dynamodb:UpdateItem"},
				"Resource": s.spec.tableARN(),
			},
			map[string]any{
				"Sid":      "WriteOwnLogs",
				"Effect":   "Allow",
				"Action":   []any{"logs:CreateLogStream", "logs:PutLogEvents"},
				"Resource": s.spec.lambdaLogGroupARN() + ":*",
			},
		},
	}
}

// policyEqual compares documents semantically. Both sides are round-tripped
// through JSON first so a policy AWS reformatted does not read as drift.
func policyEqual(current, desired map[string]any) bool {
	if current == nil {
		return false
	}

	normalise := func(v any) any {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		var out any
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil
		}
		return out
	}
	return reflect.DeepEqual(normalise(current), normalise(desired))
}
