package fleetinfra

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamotypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// registryTableStep provisions the Fleet Registry.
//
// The table is the single source of durable fleet state, so this step only ever
// creates or strengthens it: it turns protections on and adds the missing index,
// and has no path that removes anything.
type registryTableStep struct {
	c    *Clients
	spec Spec
	log  *slog.Logger

	// Populated by Plan, consumed by Apply.
	create              bool
	addGSI              bool
	enablePITR          bool
	enableSSE           bool
	enableDeletionGuard bool

	// pollInterval is how often Apply re-checks table status. Zero in tests.
	pollInterval time.Duration
}

func newRegistryTableStep(c *Clients, spec Spec, log *slog.Logger) *registryTableStep {
	return &registryTableStep{c: c, spec: spec, log: stepLogger(log, "registry table"), pollInterval: 5 * time.Second}
}

func (s *registryTableStep) Name() string { return "registry table" }

func (s *registryTableStep) Plan(ctx context.Context) (Action, error) {
	action := Action{Resource: s.Name()}

	out, err := s.c.dynamo.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(s.spec.RegistryTable),
	})
	if err != nil {
		var missing *dynamotypes.ResourceNotFoundException
		if !errors.As(err, &missing) {
			return action, fmt.Errorf("describing table: %w", err)
		}

		s.create, s.enablePITR = true, true
		action.Kind = ActionCreate
		action.Details = []string{"partition key ClusterID", gsiName + " index", "PITR", "SSE", "deletion protection"}
		return action, nil
	}

	table := out.Table
	if table == nil {
		return action, fmt.Errorf("describing table: empty description for %s", s.spec.RegistryTable)
	}

	if !hasIndex(table.GlobalSecondaryIndexes, gsiName) {
		s.addGSI = true
		action.Details = append(action.Details, "add "+gsiName)
	}
	if !aws.ToBool(table.DeletionProtectionEnabled) {
		s.enableDeletionGuard = true
		action.Details = append(action.Details, "enable deletion protection")
	}
	if table.SSEDescription == nil || table.SSEDescription.Status != dynamotypes.SSEStatusEnabled {
		s.enableSSE = true
		action.Details = append(action.Details, "enable encryption at rest")
	}

	enabled, err := s.pitrEnabled(ctx)
	if err != nil {
		return action, err
	}
	if !enabled {
		s.enablePITR = true
		action.Details = append(action.Details, "enable point-in-time recovery")
	}

	if len(action.Details) > 0 {
		action.Kind = ActionUpdate
	}
	return action, nil
}

func (s *registryTableStep) pitrEnabled(ctx context.Context) (bool, error) {
	out, err := s.c.dynamo.DescribeContinuousBackups(ctx, &dynamodb.DescribeContinuousBackupsInput{
		TableName: aws.String(s.spec.RegistryTable),
	})
	if err != nil {
		return false, fmt.Errorf("describing continuous backups: %w", err)
	}
	desc := out.ContinuousBackupsDescription
	if desc == nil || desc.PointInTimeRecoveryDescription == nil {
		return false, nil
	}
	return desc.PointInTimeRecoveryDescription.PointInTimeRecoveryStatus == dynamotypes.PointInTimeRecoveryStatusEnabled, nil
}

func (s *registryTableStep) Apply(ctx context.Context, _ Action) error {
	if s.create {
		if err := s.createTable(ctx); err != nil {
			return err
		}
	}

	// Each of these requires the table to be ACTIVE, and DynamoDB permits only
	// one structural change at a time, so they are serialised behind a wait.
	if s.addGSI || s.enableSSE || s.enableDeletionGuard {
		if err := s.waitActive(ctx); err != nil {
			return err
		}
		if err := s.updateTable(ctx); err != nil {
			return err
		}
	}

	if s.enablePITR {
		if err := s.waitActive(ctx); err != nil {
			return err
		}
		_, err := s.c.dynamo.UpdateContinuousBackups(ctx, &dynamodb.UpdateContinuousBackupsInput{
			TableName: aws.String(s.spec.RegistryTable),
			PointInTimeRecoverySpecification: &dynamotypes.PointInTimeRecoverySpecification{
				PointInTimeRecoveryEnabled: aws.Bool(true),
			},
		})
		if err != nil {
			return fmt.Errorf("enabling point-in-time recovery: %w", err)
		}
		s.log.Info("enabled point-in-time recovery", "table", s.spec.RegistryTable)
	}

	return nil
}

func (s *registryTableStep) createTable(ctx context.Context) error {
	s.log.Info("creating registry table", "table", s.spec.RegistryTable, "index", gsiName)

	_, err := s.c.dynamo.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(s.spec.RegistryTable),
		BillingMode: dynamotypes.BillingModePayPerRequest,

		// Only key and index attributes are declared; DynamoDB is schemaless for
		// everything else the registry client writes (Version, LastReportedAt,
		// LeaseHolder, LeaseExpiresAt, timestamps).
		AttributeDefinitions: []dynamotypes.AttributeDefinition{
			{AttributeName: aws.String("ClusterID"), AttributeType: dynamotypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("Provider"), AttributeType: dynamotypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("Phase"), AttributeType: dynamotypes.ScalarAttributeTypeS},
		},
		// ClusterID alone: one item per cluster, so a status report and a phase
		// transition contend on the same item and the lease actually serialises
		// them.
		KeySchema: []dynamotypes.KeySchemaElement{
			{AttributeName: aws.String("ClusterID"), KeyType: dynamotypes.KeyTypeHash},
		},
		GlobalSecondaryIndexes:    []dynamotypes.GlobalSecondaryIndex{providerPhaseIndex()},
		SSESpecification:          &dynamotypes.SSESpecification{Enabled: aws.Bool(true)},
		DeletionProtectionEnabled: aws.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("creating table: %w", err)
	}
	s.log.Info("created registry table", "table", s.spec.RegistryTable, "table_arn", s.spec.tableARN())
	return nil
}

func (s *registryTableStep) updateTable(ctx context.Context) error {
	in := &dynamodb.UpdateTableInput{TableName: aws.String(s.spec.RegistryTable)}

	if s.addGSI {
		idx := providerPhaseIndex()
		in.AttributeDefinitions = []dynamotypes.AttributeDefinition{
			{AttributeName: aws.String("Provider"), AttributeType: dynamotypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("Phase"), AttributeType: dynamotypes.ScalarAttributeTypeS},
		}
		in.GlobalSecondaryIndexUpdates = []dynamotypes.GlobalSecondaryIndexUpdate{{
			Create: &dynamotypes.CreateGlobalSecondaryIndexAction{
				IndexName:  idx.IndexName,
				KeySchema:  idx.KeySchema,
				Projection: idx.Projection,
			},
		}}
	}
	if s.enableSSE {
		in.SSESpecification = &dynamotypes.SSESpecification{Enabled: aws.Bool(true)}
	}
	if s.enableDeletionGuard {
		in.DeletionProtectionEnabled = aws.Bool(true)
	}

	if _, err := s.c.dynamo.UpdateTable(ctx, in); err != nil {
		return fmt.Errorf("updating table: %w", err)
	}
	s.log.Info("updated registry table",
		"table", s.spec.RegistryTable,
		"added_index", s.addGSI,
		"enabled_encryption", s.enableSSE,
		"enabled_deletion_protection", s.enableDeletionGuard)
	return nil
}

// waitActive blocks until the table leaves CREATING/UPDATING.
func (s *registryTableStep) waitActive(ctx context.Context) error {
	for {
		out, err := s.c.dynamo.DescribeTable(ctx, &dynamodb.DescribeTableInput{
			TableName: aws.String(s.spec.RegistryTable),
		})
		if err != nil {
			return fmt.Errorf("waiting for table to become active: %w", err)
		}
		if out.Table != nil && out.Table.TableStatus == dynamotypes.TableStatusActive {
			return nil
		}

		s.log.Debug("waiting for table to become active",
			"table", s.spec.RegistryTable, "retry_in", s.pollInterval)

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for table to become active: %w", ctx.Err())
		case <-time.After(s.pollInterval):
		}
	}
}

// providerPhaseIndex is created with the table rather than later: `fleet audit`
// and `fleet update` enumerate by provider and phase, and adding a GSI to a
// populated table is a slow online backfill.
func providerPhaseIndex() dynamotypes.GlobalSecondaryIndex {
	return dynamotypes.GlobalSecondaryIndex{
		IndexName: aws.String(gsiName),
		KeySchema: []dynamotypes.KeySchemaElement{
			{AttributeName: aws.String("Provider"), KeyType: dynamotypes.KeyTypeHash},
			{AttributeName: aws.String("Phase"), KeyType: dynamotypes.KeyTypeRange},
		},
		Projection: &dynamotypes.Projection{ProjectionType: dynamotypes.ProjectionTypeAll},
	}
}

func hasIndex(indexes []dynamotypes.GlobalSecondaryIndexDescription, name string) bool {
	for _, idx := range indexes {
		if aws.ToString(idx.IndexName) == name {
			return true
		}
	}
	return false
}
