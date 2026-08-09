//go:build integration

package registry

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// The registry contract, run against a real DynamoDB rather than the in-memory
// implementation. The conditional-write semantics the lease depends on cannot
// be proven by a fake alone — this is where "the condition expression is
// actually correct" gets tested.
//
// Requires DynamoDB Local:
//
//	docker run -p 8000:8000 amazon/dynamodb-local
//	KUBESPIN_DYNAMODB_ENDPOINT=http://localhost:8000 make integration
func TestDynamoDBRegistry(t *testing.T) {
	endpoint := os.Getenv("KUBESPIN_DYNAMODB_ENDPOINT")
	if endpoint == "" {
		t.Skip("KUBESPIN_DYNAMODB_ENDPOINT is not set; skipping DynamoDB integration tests")
	}

	client := newLocalClient(t, endpoint)

	runContract(t, func(t *testing.T, clock *fakeClock) Registry {
		table := createTestTable(t, client)
		return &DynamoDB{client: client, table: table, now: clock.Now}
	})
}

func newLocalClient(t *testing.T, endpoint string) *dynamodb.Client {
	t.Helper()

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		// DynamoDB Local accepts any credentials but requires them to be present.
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("local", "local", "")),
	)
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}

	return dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})
}

// createTestTable builds a table matching what `fleet bootstrap` provisions,
// and registers its deletion. Each subtest gets its own so they cannot interfere.
func createTestTable(t *testing.T, client *dynamodb.Client) string {
	t.Helper()

	name := fmt.Sprintf("kubespin-test-%s-%d",
		strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()), time.Now().UnixNano())
	if len(name) > 255 {
		name = name[:255]
	}

	ctx := context.Background()
	_, err := client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(name),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String(attrClusterID), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String(attrProvider), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String(attrPhase), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String(attrClusterID), KeyType: types.KeyTypeHash},
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
			IndexName: aws.String(gsiProviderPhase),
			KeySchema: []types.KeySchemaElement{
				{AttributeName: aws.String(attrProvider), KeyType: types.KeyTypeHash},
				{AttributeName: aws.String(attrPhase), KeyType: types.KeyTypeRange},
			},
			Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
		}},
	})
	if err != nil {
		t.Fatalf("creating table %s: %v", name, err)
	}

	if err := dynamodb.NewTableExistsWaiter(client).Wait(ctx,
		&dynamodb.DescribeTableInput{TableName: aws.String(name)}, 30*time.Second); err != nil {
		t.Fatalf("waiting for table %s: %v", name, err)
	}

	t.Cleanup(func() {
		if _, err := client.DeleteTable(context.Background(),
			&dynamodb.DeleteTableInput{TableName: aws.String(name)}); err != nil {
			t.Logf("deleting table %s: %v", name, err)
		}
	})

	return name
}
