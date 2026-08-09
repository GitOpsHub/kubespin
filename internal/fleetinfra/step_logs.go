package fleetinfra

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	logstypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

// logGroupsStep provisions both log groups up front: the Lambda's and the API's.
//
// Creating them here rather than letting AWS create them implicitly is what lets
// retention be set at all — an implicitly created group retains forever, and the
// Lambda's execution policy is scoped to a group that must already exist.
type logGroupsStep struct {
	c    *Clients
	spec Spec

	pending []logGroupFix
}

type logGroupFix struct {
	name         string
	create       bool
	setRetention bool
}

func newLogGroupsStep(c *Clients, spec Spec) *logGroupsStep {
	return &logGroupsStep{c: c, spec: spec}
}

func (s *logGroupsStep) Name() string { return "log groups" }

func (s *logGroupsStep) Plan(ctx context.Context) (Action, error) {
	action := Action{Resource: "log groups"}

	for _, name := range []string{s.spec.lambdaLogGroup(), s.spec.apiLogGroup()} {
		group, err := s.find(ctx, name)
		if err != nil {
			return action, err
		}

		switch {
		case group == nil:
			s.pending = append(s.pending, logGroupFix{name: name, create: true, setRetention: true})
			action.Details = append(action.Details, "create "+name)
		case aws.ToInt32(group.RetentionInDays) != s.spec.LogRetentionDays:
			s.pending = append(s.pending, logGroupFix{name: name, setRetention: true})
			action.Details = append(action.Details,
				fmt.Sprintf("set %s retention to %d days", name, s.spec.LogRetentionDays))
		}
	}

	switch {
	case len(s.pending) == 0:
		action.Kind = ActionNone
	case len(s.pending) == 2 && s.pending[0].create && s.pending[1].create:
		action.Kind = ActionCreate
	default:
		action.Kind = ActionUpdate
	}
	return action, nil
}

// find returns the log group with exactly this name, or nil. DescribeLogGroups
// matches on prefix, so the exact name still has to be checked.
func (s *logGroupsStep) find(ctx context.Context, name string) (*logstypes.LogGroup, error) {
	out, err := s.c.logs.DescribeLogGroups(ctx, &cloudwatchlogs.DescribeLogGroupsInput{
		LogGroupNamePrefix: aws.String(name),
	})
	if err != nil {
		return nil, fmt.Errorf("describing log group %s: %w", name, err)
	}

	for i, group := range out.LogGroups {
		if aws.ToString(group.LogGroupName) == name {
			return &out.LogGroups[i], nil
		}
	}
	return nil, nil
}

func (s *logGroupsStep) Apply(ctx context.Context, _ Action) error {
	for _, fix := range s.pending {
		if fix.create {
			_, err := s.c.logs.CreateLogGroup(ctx, &cloudwatchlogs.CreateLogGroupInput{
				LogGroupName: aws.String(fix.name),
			})
			if err != nil {
				return fmt.Errorf("creating log group %s: %w", fix.name, err)
			}
		}
		if fix.setRetention {
			_, err := s.c.logs.PutRetentionPolicy(ctx, &cloudwatchlogs.PutRetentionPolicyInput{
				LogGroupName:    aws.String(fix.name),
				RetentionInDays: aws.Int32(s.spec.LogRetentionDays),
			})
			if err != nil {
				return fmt.Errorf("setting retention on %s: %w", fix.name, err)
			}
		}
	}
	return nil
}
