// Command ingestion is the Central Ingestion API handler: the only inbound
// surface in the system, and the endpoint every cluster's fleet-status-reporter
// pushes to.
//
// It is deliberately not implemented. This skeleton exists so the endpoint, its
// role, and its log group are real from M0, giving cluster egress allowlists a
// stable destination to permit during M2.
//
// M6 fills it in:
//   - verify the caller's workload identity token per cloud (IRSA, GCP Workload
//     Identity, Azure federated credential), each with its own issuer;
//   - bind the token's subject to the {clusterId} in the request path, so a
//     signature issued to cluster A cannot be replayed to report status as
//     cluster B;
//   - conditionally update that cluster's item in the Fleet Registry.
package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

type errorBody struct {
	Error     string `json:"error"`
	Message   string `json:"message"`
	ClusterID string `json:"clusterId,omitempty"`
}

func handler(_ context.Context, req events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	body, err := json.Marshal(errorBody{
		Error:     "not_implemented",
		Message:   "Central Ingestion API is scaffolded but not implemented (milestone M6).",
		ClusterID: req.PathParameters["clusterId"],
	})
	if err != nil {
		// Marshalling a fixed struct cannot realistically fail; degrade rather
		// than return an error the caller would see as a 502.
		body = []byte(`{"error":"not_implemented"}`)
	}

	return events.APIGatewayV2HTTPResponse{
		StatusCode: http.StatusNotImplemented,
		Headers:    map[string]string{"content-type": "application/json"},
		Body:       string(body),
	}, nil
}

func main() {
	lambda.Start(handler)
}
