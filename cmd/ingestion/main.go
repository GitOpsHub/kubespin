// Command ingestion is the Central Ingestion API handler: the only inbound
// surface in the system, and the endpoint every cluster's
// fleet-status-reporter pushes to.
//
// It verifies the caller's workload identity token against the specific
// OIDC issuer recorded for the {clusterId} in the request path — see
// internal/ingestion for why that binding is what stops a signature issued
// to one cluster from being replayed to spoof another — then records the
// push in the Fleet Registry.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/ingestion"
	"github.com/GitOpsHub/kubespin/internal/registry"
)

func newHandler(ctx context.Context) (*ingestion.Handler, error) {
	dsn := os.Getenv("REGISTRY_DSN")

	reg, err := registry.NewPostgres(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to the Fleet Registry: %w", err)
	}

	verifier := ingestion.NewVerifier(ingestion.NewJWKSResolver(nil))
	return ingestion.NewHandler(reg, verifier), nil
}

func handleRequest(h *ingestion.Handler) func(context.Context, events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	return func(ctx context.Context, req events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
		clusterID := core.ClusterID(req.PathParameters["clusterId"])
		token := bearerToken(req.Headers)

		code, resp := h.HandleStatus(ctx, clusterID, token, []byte(req.Body))

		body, err := json.Marshal(resp)
		if err != nil {
			// Marshalling a plain struct of strings and a bool cannot
			// realistically fail; degrade rather than return an error the
			// caller would see as a 502.
			body = []byte(`{"error":"internal_error"}`)
			code = http.StatusInternalServerError
		}

		return events.APIGatewayV2HTTPResponse{
			StatusCode: code,
			Headers:    map[string]string{"content-type": "application/json"},
			Body:       string(body),
		}, nil
	}
}

// bearerToken extracts the token from "Authorization: Bearer <token>",
// tolerating API Gateway's lower-cased header keys.
func bearerToken(headers map[string]string) string {
	for key, value := range headers {
		if !strings.EqualFold(key, "authorization") {
			continue
		}
		const prefix = "Bearer "
		if len(value) > len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
			return value[len(prefix):]
		}
	}
	return ""
}

func main() {
	// JSON, because everything this handler writes lands in CloudWatch Logs,
	// where structured fields are queryable and a free-text line is not.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx := context.Background()

	h, err := newHandler(ctx)
	if err != nil {
		logger.Error("ingestion handler failed to start", "error", err)
		os.Exit(1)
	}

	// REGISTRY_DSN itself is never logged: it carries the Postgres password.
	logger.Info("ingestion handler started")

	lambda.Start(handleRequest(h))
}
