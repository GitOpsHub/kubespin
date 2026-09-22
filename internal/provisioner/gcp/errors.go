package gcp

import (
	"errors"

	"google.golang.org/api/googleapi"
)

// code extracts the HTTP status code from a googleapi error, or 0 if err is
// not one. REST-based GCP clients (IAM, Compute) report errors this way,
// unlike the gRPC-based GKE client which uses grpc/status codes instead.
func code(err error) int {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return gerr.Code
	}
	return 0
}
