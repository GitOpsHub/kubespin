package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// gsiProviderPhase is created with the table by `fleet bootstrap`.
const gsiProviderPhase = "ProviderPhaseIndex"

// dynamoAPI lists only the calls this package makes, so the registry is
// testable without credentials and the permission surface is explicit.
type dynamoAPI interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	Scan(context.Context, *dynamodb.ScanInput, ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
}

// DynamoDB is the production Registry, backed by the Fleet Registry table.
type DynamoDB struct {
	client dynamoAPI
	table  string
	now    func() time.Time
}

// NewDynamoDB builds a registry client against the named table.
func NewDynamoDB(ctx context.Context, region, table string) (*DynamoDB, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	return &DynamoDB{client: dynamodb.NewFromConfig(cfg), table: table, now: time.Now}, nil
}

// Get returns a cluster's record.
func (d *DynamoDB) Get(ctx context.Context, id core.ClusterID) (Record, error) {
	out, err := d.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(d.table),
		Key:       key(id),
		// Provisioning decisions are made from this read, so a stale replica is
		// not good enough.
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return Record{}, fmt.Errorf("getting cluster %s: %w", id, err)
	}
	if len(out.Item) == 0 {
		return Record{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return unmarshalRecord(out.Item)
}

// Create registers a new cluster.
func (d *DynamoDB) Create(ctx context.Context, rec Record) (Record, error) {
	if err := rec.Validate(); err != nil {
		return Record{}, err
	}
	if rec.Version == 0 {
		rec.Version = 1
	}

	_, err := d.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(d.table),
		Item:                marshalRecord(rec),
		ConditionExpression: aws.String("attribute_not_exists(#id)"),
		ExpressionAttributeNames: map[string]string{
			"#id": attrClusterID,
		},
	})
	if err != nil {
		if conditionFailed(err) {
			return Record{}, fmt.Errorf("%w: %s", ErrAlreadyExists, rec.ClusterID)
		}
		return Record{}, fmt.Errorf("creating cluster %s: %w", rec.ClusterID, err)
	}
	return rec, nil
}

// UpdatePhase advances a cluster to its next phase.
func (d *DynamoDB) UpdatePhase(ctx context.Context, rec Record, to core.Phase) (Record, error) {
	// Rejected here rather than at the storage layer's mercy: an illegal
	// transition must never reach the table.
	if err := core.ValidateTransition(rec.Phase, to); err != nil {
		return Record{}, fmt.Errorf("advancing %s: %w", rec.ClusterID, err)
	}

	out, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(d.table),
		Key:              key(rec.ClusterID),
		UpdateExpression: aws.String("SET #phase = :to, #version = :next, #updated = :now"),
		// Both the phase and the version are asserted: a racing writer that
		// advanced the record loses this write instead of silently overwriting.
		ConditionExpression: aws.String("attribute_exists(#id) AND #version = :current AND #phase = :from"),
		ExpressionAttributeNames: map[string]string{
			"#id":      attrClusterID,
			"#phase":   attrPhase,
			"#version": attrVersion,
			"#updated": attrUpdatedAt,
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":to":      &types.AttributeValueMemberS{Value: to.String()},
			":from":    &types.AttributeValueMemberS{Value: rec.Phase.String()},
			":current": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", rec.Version)},
			":next":    &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", rec.Version+1)},
			":now":     &types.AttributeValueMemberS{Value: d.now().UTC().Format(time.RFC3339Nano)},
		},
		ReturnValues: types.ReturnValueAllNew,
		// Returns the conflicting item on failure, so not-found and
		// version-conflict are distinguished without a second read.
		ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
	})
	if err != nil {
		if item, failed := conditionFailure(err); failed {
			if len(item) == 0 {
				return Record{}, fmt.Errorf("%w: %s", ErrNotFound, rec.ClusterID)
			}
			return Record{}, fmt.Errorf("%w: %s expected phase %s version %d",
				ErrVersionConflict, rec.ClusterID, rec.Phase, rec.Version)
		}
		return Record{}, fmt.Errorf("updating phase for %s: %w", rec.ClusterID, err)
	}

	return unmarshalRecord(out.Attributes)
}

// Touch records a status report.
func (d *DynamoDB) Touch(ctx context.Context, id core.ClusterID, at time.Time) error {
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(d.table),
		Key:              key(id),
		UpdateExpression: aws.String("SET #reported = :at"),
		// No version assertion: reports arrive every couple of minutes from
		// every cluster and must not contend with provisioning writes.
		ConditionExpression: aws.String("attribute_exists(#id)"),
		ExpressionAttributeNames: map[string]string{
			"#id":       attrClusterID,
			"#reported": attrLastReportedAt,
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":at": &types.AttributeValueMemberS{Value: at.UTC().Format(time.RFC3339Nano)},
		},
	})
	if err != nil {
		if conditionFailed(err) {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return fmt.Errorf("recording status for %s: %w", id, err)
	}
	return nil
}

// List returns records matching filter.
//
// A provider filter is served by the ProviderPhaseIndex GSI; without one there
// is no alternative to a scan, which is why fleet-wide operations are expected
// to filter by provider.
func (d *DynamoDB) List(ctx context.Context, filter Filter) ([]Record, error) {
	if filter.Provider != "" {
		return d.queryIndex(ctx, filter)
	}
	return d.scan(ctx, filter)
}

func (d *DynamoDB) queryIndex(ctx context.Context, filter Filter) ([]Record, error) {
	condition := "#provider = :provider"
	names := map[string]string{"#provider": attrProvider}
	values := map[string]types.AttributeValue{
		":provider": &types.AttributeValueMemberS{Value: filter.Provider.String()},
	}
	if filter.Phase != "" {
		condition += " AND #phase = :phase"
		names["#phase"] = attrPhase
		values[":phase"] = &types.AttributeValueMemberS{Value: filter.Phase.String()}
	}

	var records []Record
	var start map[string]types.AttributeValue
	for {
		out, err := d.client.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String(d.table),
			IndexName:                 aws.String(gsiProviderPhase),
			KeyConditionExpression:    aws.String(condition),
			ExpressionAttributeNames:  names,
			ExpressionAttributeValues: values,
			ExclusiveStartKey:         start,
		})
		if err != nil {
			return nil, fmt.Errorf("querying %s: %w", gsiProviderPhase, err)
		}

		parsed, err := unmarshalAll(out.Items)
		if err != nil {
			return nil, err
		}
		records = append(records, parsed...)

		if start = out.LastEvaluatedKey; len(start) == 0 {
			return records, nil
		}
	}
}

func (d *DynamoDB) scan(ctx context.Context, filter Filter) ([]Record, error) {
	in := &dynamodb.ScanInput{TableName: aws.String(d.table)}
	if filter.Phase != "" {
		in.FilterExpression = aws.String("#phase = :phase")
		in.ExpressionAttributeNames = map[string]string{"#phase": attrPhase}
		in.ExpressionAttributeValues = map[string]types.AttributeValue{
			":phase": &types.AttributeValueMemberS{Value: filter.Phase.String()},
		}
	}

	var records []Record
	for {
		out, err := d.client.Scan(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("scanning registry: %w", err)
		}

		parsed, err := unmarshalAll(out.Items)
		if err != nil {
			return nil, err
		}
		records = append(records, parsed...)

		if in.ExclusiveStartKey = out.LastEvaluatedKey; len(in.ExclusiveStartKey) == 0 {
			return records, nil
		}
	}
}

func unmarshalAll(items []map[string]types.AttributeValue) ([]Record, error) {
	out := make([]Record, 0, len(items))
	for _, item := range items {
		rec, err := unmarshalRecord(item)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// AcquireLease claims a cluster for holder.
func (d *DynamoDB) AcquireLease(ctx context.Context, id core.ClusterID, holder string, ttl time.Duration) (Lease, error) {
	if holder == "" {
		return Lease{}, fmt.Errorf("%w: lease holder is required", core.ErrInvalidSpec)
	}

	now := d.now()
	lease := Lease{Holder: holder, ExpiresAt: now.Add(ttl)}

	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(d.table),
		Key:              key(id),
		UpdateExpression: aws.String("SET #holder = :holder, #expires = :expires"),
		// Free, expired, or already ours. The expiry comparison is what makes a
		// crashed run self-heal instead of wedging the cluster forever.
		ConditionExpression: aws.String(
			"attribute_exists(#id) AND (attribute_not_exists(#holder) OR #expires < :now OR #holder = :holder)"),
		ExpressionAttributeNames: map[string]string{
			"#id":      attrClusterID,
			"#holder":  attrLeaseHolder,
			"#expires": attrLeaseExpiresAt,
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":holder":  &types.AttributeValueMemberS{Value: holder},
			":expires": &types.AttributeValueMemberN{Value: epochMillis(lease.ExpiresAt)},
			":now":     &types.AttributeValueMemberN{Value: epochMillis(now)},
		},
		ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
	})
	if err != nil {
		if item, failed := conditionFailure(err); failed {
			return Lease{}, leaseConflict(id, item, ErrLeaseHeld)
		}
		return Lease{}, fmt.Errorf("acquiring lease on %s: %w", id, err)
	}
	return lease, nil
}

// RenewLease extends a lease the caller still holds.
func (d *DynamoDB) RenewLease(ctx context.Context, id core.ClusterID, holder string, ttl time.Duration) (Lease, error) {
	now := d.now()
	lease := Lease{Holder: holder, ExpiresAt: now.Add(ttl)}

	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(d.table),
		Key:              key(id),
		UpdateExpression: aws.String("SET #expires = :expires"),
		// Strictly greater than now: an expired lease cannot be renewed, because
		// another holder may already own it.
		ConditionExpression: aws.String("attribute_exists(#id) AND #holder = :holder AND #expires > :now"),
		ExpressionAttributeNames: map[string]string{
			"#id":      attrClusterID,
			"#holder":  attrLeaseHolder,
			"#expires": attrLeaseExpiresAt,
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":holder":  &types.AttributeValueMemberS{Value: holder},
			":expires": &types.AttributeValueMemberN{Value: epochMillis(lease.ExpiresAt)},
			":now":     &types.AttributeValueMemberN{Value: epochMillis(now)},
		},
		ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
	})
	if err != nil {
		if item, failed := conditionFailure(err); failed {
			return Lease{}, leaseConflict(id, item, ErrLeaseLost)
		}
		return Lease{}, fmt.Errorf("renewing lease on %s: %w", id, err)
	}
	return lease, nil
}

// ReleaseLease drops a lease the caller holds.
func (d *DynamoDB) ReleaseLease(ctx context.Context, id core.ClusterID, holder string) error {
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(d.table),
		Key:                 key(id),
		UpdateExpression:    aws.String("REMOVE #holder, #expires"),
		ConditionExpression: aws.String("attribute_exists(#id) AND #holder = :holder"),
		ExpressionAttributeNames: map[string]string{
			"#id":      attrClusterID,
			"#holder":  attrLeaseHolder,
			"#expires": attrLeaseExpiresAt,
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":holder": &types.AttributeValueMemberS{Value: holder},
		},
		ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
	})
	if err != nil {
		if item, failed := conditionFailure(err); failed {
			return leaseConflict(id, item, ErrLeaseLost)
		}
		return fmt.Errorf("releasing lease on %s: %w", id, err)
	}
	return nil
}

// leaseConflict distinguishes "no such cluster" from a genuine lease conflict,
// using the item DynamoDB returns alongside the failed condition.
func leaseConflict(id core.ClusterID, item map[string]types.AttributeValue, sentinel error) error {
	if len(item) == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if holder := stringAttr(item, attrLeaseHolder); holder != "" {
		return fmt.Errorf("%w: %s is held by %s", sentinel, id, holder)
	}
	return fmt.Errorf("%w: %s", sentinel, id)
}

func conditionFailed(err error) bool {
	_, failed := conditionFailure(err)
	return failed
}

func conditionFailure(err error) (map[string]types.AttributeValue, bool) {
	var failed *types.ConditionalCheckFailedException
	if errors.As(err, &failed) {
		return failed.Item, true
	}
	return nil, false
}
