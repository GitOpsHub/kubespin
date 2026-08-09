package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/registry"
)

// ExpectedSubject is the Kubernetes service account subject every
// fleet-status-reporter token must present. It matches
// provisioner.StatusReporter()'s namespace/name — the same component every
// IdentityProvisioner implementation binds workload identity to in M2 — so a
// token from any other in-cluster workload is rejected regardless of how it
// was signed.
const ExpectedSubject = "system:serviceaccount:kubespin-system:fleet-status-reporter"

// ExpectedAudience is the audience fleet-status-reporter must request when it
// projects its token, scoping the token to this API and nothing else — a
// token minted for some other audience proves identity to that audience, not
// to this one.
const ExpectedAudience = "kubespin-ingestion"

// StatusPayload is the compact status fleet-status-reporter pushes: a
// summary of what its local Argo CD reports, not the full application list.
type StatusPayload struct {
	SyncedApps   int    `json:"syncedApps"`
	HealthyApps  int    `json:"healthyApps"`
	DegradedApps int    `json:"degradedApps"`
	CommitSHA    string `json:"commitSha"`
}

// Response is HandleStatus's result body, on both success and failure.
type Response struct {
	Accepted  bool   `json:"accepted"`
	ClusterID string `json:"clusterId,omitempty"`
	Error     string `json:"error,omitempty"`
	Message   string `json:"message,omitempty"`
}

// Handler implements the Central Ingestion API's one operation: accept a
// signed status push from a cluster's fleet-status-reporter.
type Handler struct {
	reg      registry.Registry
	verifier *Verifier
	now      func() time.Time
	logger   *slog.Logger
}

// NewHandler builds a Handler.
func NewHandler(reg registry.Registry, verifier *Verifier, opts ...Option) *Handler {
	o := resolve(opts)
	return &Handler{reg: reg, verifier: verifier, now: time.Now, logger: o.logger}
}

// HandleStatus verifies token proves the caller is clusterID's
// fleet-status-reporter, then records the push in the Fleet Registry.
//
// Every failure mode returns a distinct status code and machine-readable
// Error rather than a bare 500, because "your token was rejected" and "we
// could not reach the registry" call for different operator responses.
func (h *Handler) HandleStatus(ctx context.Context, clusterID core.ClusterID, token string, body []byte) (int, Response) {
	logger := loggerOr(h.logger)

	if token == "" {
		logger.Warn("rejected status push: no bearer token", "cluster", clusterID)
		return 401, errResponse(clusterID, "missing_token", "Authorization bearer token is required")
	}

	rec, err := h.reg.Get(ctx, clusterID)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			logger.Warn("rejected status push: cluster is not registered", "cluster", clusterID)
			return 404, errResponse(clusterID, "unknown_cluster", "cluster is not registered in the Fleet Registry")
		}
		logger.Error("could not read the Fleet Registry", "cluster", clusterID, "error", err)
		return 500, errResponse(clusterID, "registry_error", "could not read the Fleet Registry")
	}

	claims, err := h.verifier.Verify(ctx, token, rec.OIDCIssuer)
	if err != nil {
		logger.Warn("rejected status push: token failed verification",
			"cluster", clusterID, "issuer", rec.OIDCIssuer, "error", err)
		return 403, errResponse(clusterID, "invalid_token", err.Error())
	}

	// Binding the token to this cluster's issuer (above) is what stops a
	// signature from cluster A being replayed to spoof cluster B. Subject and
	// audience are a second, independent check: they stop any *other*
	// in-cluster workload on the *same* cluster — one that also has a token
	// from the same issuer — from pushing status it has no business pushing.
	if claims.Subject != ExpectedSubject {
		logger.Warn("rejected status push: unexpected token subject",
			"cluster", clusterID, "subject", claims.Subject)
		return 403, errResponse(clusterID, "wrong_subject", "token subject does not identify fleet-status-reporter")
	}
	if !slices.Contains(claims.Audience, ExpectedAudience) {
		logger.Warn("rejected status push: unexpected token audience",
			"cluster", clusterID, "audience", claims.Audience)
		return 403, errResponse(clusterID, "wrong_audience", "token was not issued for the ingestion API")
	}

	var payload StatusPayload
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			logger.Warn("rejected status push: payload is not valid JSON", "cluster", clusterID)
			return 400, errResponse(clusterID, "invalid_body", "status payload is not valid JSON")
		}
	}

	if err := h.reg.Touch(ctx, clusterID, h.now()); err != nil {
		logger.Error("could not record the status push", "cluster", clusterID, "error", err)
		return 500, errResponse(clusterID, "registry_error", "could not record the status push")
	}

	logger.Info("recorded status push",
		"cluster", clusterID, "synced", payload.SyncedApps, "healthy", payload.HealthyApps,
		"degraded", payload.DegradedApps, "commit", payload.CommitSHA)
	return 202, Response{Accepted: true, ClusterID: clusterID.String()}
}

func errResponse(clusterID core.ClusterID, code, message string) Response {
	return Response{ClusterID: clusterID.String(), Error: code, Message: message}
}
