// Package fleetinfra provisions the shared fleet infrastructure — the Central
// Ingestion API — directly through the AWS SDK. The Fleet Registry itself is a
// Postgres database (internal/registry) the operator supplies connection
// details for; it self-migrates its schema on connect and is not provisioned
// here.
//
// There is no state file. Every step describes live state and diffs it against
// desired state, so a run against already-provisioned infrastructure must report
// no changes. That convergence property is what a state file would otherwise
// buy, and it is asserted in the tests rather than assumed.
//
// No step ever deletes. Tearing down fleet infrastructure is a deliberate manual
// act, not something a converge run can do by accident.
package fleetinfra

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// ErrSpec wraps configuration problems; ErrAccountMismatch is returned when the
// caller's credentials do not belong to the configured fleet account.
var (
	ErrSpec            = errors.New("invalid fleet infrastructure spec")
	ErrAccountMismatch = errors.New("caller account does not match the configured fleet account")
)

// Defaults for the tunable parts of the stack.
const (
	DefaultNamePrefix       = "kubespin"
	DefaultLogRetentionDays = 30
	DefaultThrottleBurst    = 100
	DefaultThrottleRate     = 50

	// StatusRouteKey is the only route on the ingestion API. The clusterId in
	// the path is what M6 binds the caller's token subject against.
	StatusRouteKey  = "POST /v1/clusters/{clusterId}/status"
	statusRoutePath = "/v1/clusters/{clusterId}/status"
)

var accountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)

// Spec is the desired state of the fleet infrastructure.
type Spec struct {
	// AccountID is the fleet account. It is checked against the caller's real
	// identity before anything is provisioned.
	AccountID string
	Region    string

	NamePrefix       string
	RegistryDSN      string
	LogRetentionDays int32
	ThrottleBurst    int32
	ThrottleRate     float64

	// LambdaZip is the packaged ingestion handler, built by PackageLambda.
	LambdaZip []byte
}

// withDefaults returns a copy with unset tunables filled in.
func (s Spec) withDefaults() Spec {
	if s.NamePrefix == "" {
		s.NamePrefix = DefaultNamePrefix
	}
	if s.LogRetentionDays == 0 {
		s.LogRetentionDays = DefaultLogRetentionDays
	}
	if s.ThrottleBurst == 0 {
		s.ThrottleBurst = DefaultThrottleBurst
	}
	if s.ThrottleRate == 0 {
		s.ThrottleRate = DefaultThrottleRate
	}
	return s
}

// Validate reports every problem with the spec at once.
func (s Spec) Validate() error {
	var errs []error
	if !accountIDPattern.MatchString(s.AccountID) {
		errs = append(errs, fmt.Errorf("%w: account id %q must be 12 digits", ErrSpec, s.AccountID))
	}
	if s.Region == "" {
		errs = append(errs, fmt.Errorf("%w: region is required", ErrSpec))
	}
	if s.RegistryDSN == "" {
		errs = append(errs, fmt.Errorf("%w: registry DSN is required", ErrSpec))
	}
	if len(s.LambdaZip) == 0 {
		errs = append(errs, fmt.Errorf("%w: packaged lambda is required", ErrSpec))
	}
	return errors.Join(errs...)
}

// Derived resource names and ARNs. The partition is assumed to be "aws";
// GovCloud and China would need this threaded through the spec.
func (s Spec) functionName() string   { return s.NamePrefix + "-ingestion" }
func (s Spec) roleName() string       { return s.NamePrefix + "-ingestion" }
func (s Spec) apiName() string        { return s.NamePrefix + "-ingestion" }
func (s Spec) lambdaLogGroup() string { return "/aws/lambda/" + s.functionName() }
func (s Spec) apiLogGroup() string    { return "/aws/apigateway/" + s.apiName() }

func (s Spec) roleARN() string {
	return fmt.Sprintf("arn:aws:iam::%s:role/%s", s.AccountID, s.roleName())
}

func (s Spec) functionARN() string {
	return fmt.Sprintf("arn:aws:lambda:%s:%s:function:%s", s.Region, s.AccountID, s.functionName())
}

// invokeARN is the API Gateway integration URI form, not the function ARN.
func (s Spec) invokeARN() string {
	return fmt.Sprintf("arn:aws:apigateway:%s:lambda:path/2015-03-31/functions/%s/invocations",
		s.Region, s.functionARN())
}

func (s Spec) lambdaLogGroupARN() string {
	return fmt.Sprintf("arn:aws:logs:%s:%s:log-group:%s", s.Region, s.AccountID, s.lambdaLogGroup())
}

func (s Spec) apiLogGroupARN() string {
	return fmt.Sprintf("arn:aws:logs:%s:%s:log-group:%s", s.Region, s.AccountID, s.apiLogGroup())
}

// ActionKind is what a step intends to do.
type ActionKind int

// Converge never deletes, so there is no ActionDelete.
const (
	ActionNone ActionKind = iota
	ActionCreate
	ActionUpdate
)

func (k ActionKind) String() string {
	switch k {
	case ActionCreate:
		return "create"
	case ActionUpdate:
		return "update"
	default:
		return "in sync"
	}
}

// Action is one step's verdict on one resource.
type Action struct {
	Resource string
	Kind     ActionKind
	// Details explains what differs, and is printed on both dry and real runs.
	Details []string
}

func (a Action) String() string {
	if len(a.Details) == 0 {
		return fmt.Sprintf("%-24s %s", a.Resource, a.Kind)
	}
	return fmt.Sprintf("%-24s %s (%s)", a.Resource, a.Kind, strings.Join(a.Details, "; "))
}

// options carries per-run configuration set through Option values.
type options struct {
	logger *slog.Logger
}

// Option configures a converge run. Options are variadic and appended to
// Converge's existing parameters, so callers that pass none are unaffected.
type Option func(*options)

// WithLogger sets the logger the converge run narrates itself through.
// A nil logger is ignored, leaving slog.Default() in place.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) {
		if logger != nil {
			o.logger = logger
		}
	}
}

// stepLogger tags a logger with the step it belongs to, tolerating a nil logger
// so a step constructed outside Converge (in a test, say) still logs safely.
func stepLogger(logger *slog.Logger, name string) *slog.Logger {
	if logger == nil {
		logger = slog.Default().With("component", "fleetinfra")
	}
	return logger.With("step", name)
}

func resolveOptions(opts []Option) options {
	cfg := options{logger: slog.Default()}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// step is one resource's converge logic. Plan must be strictly read-only: it is
// what --dry-run runs, and the only difference between a dry and a real run is
// whether Apply is then called.
type step interface {
	Name() string
	Plan(ctx context.Context) (Action, error)
	Apply(ctx context.Context, a Action) error
}

// Report is the outcome of a converge run.
type Report struct {
	DryRun  bool
	Actions []Action
	// IngestionURL is the endpoint clusters push status to, and the host every
	// cluster's egress allowlist must permit. Empty on a dry run that would
	// still have to create the API.
	IngestionURL string
}

// Changed counts resources that were not already in sync.
func (r Report) Changed() int {
	n := 0
	for _, a := range r.Actions {
		if a.Kind != ActionNone {
			n++
		}
	}
	return n
}

// Converge brings the fleet infrastructure to match spec.
//
// When dryRun is set, no mutating call is made: only each step's read-only Plan
// runs. Steps execute in dependency order and stop at the first error, so a
// failure leaves earlier resources created and later ones untouched — re-running
// resumes, because every step is create-or-update.
func Converge(ctx context.Context, c *Clients, spec Spec, dryRun bool, opts ...Option) (Report, error) {
	logger := resolveOptions(opts).logger.With("component", "fleetinfra")

	spec = spec.withDefaults()
	if err := spec.Validate(); err != nil {
		return Report{}, err
	}
	if err := c.verifyAccount(ctx, spec.AccountID); err != nil {
		return Report{}, err
	}

	logger.Info("converge starting",
		"account_id", spec.AccountID,
		"region", spec.Region,
		"name_prefix", spec.NamePrefix,
		"dry_run", dryRun)

	api := newAPIStep(c, spec, logger)
	steps := []step{
		newLogGroupsStep(c, spec, logger),
		newRoleStep(c, spec, logger),
		newFunctionStep(c, spec, logger),
		api,
		newPermissionStep(c, spec, api, logger),
	}

	report := Report{DryRun: dryRun}
	for _, s := range steps {
		stepLog := logger.With("step", s.Name())

		action, err := s.Plan(ctx)
		if err != nil {
			stepLog.Error("plan failed", "error", err)
			return report, fmt.Errorf("planning %s: %w", s.Name(), err)
		}
		report.Actions = append(report.Actions, action)

		// Debug rather than Info: a converged run must stay quiet, and this is
		// the line that makes "no changes" verifiable rather than merely claimed.
		if action.Kind == ActionNone {
			stepLog.Debug("already converged", "resource", action.Resource)
			continue
		}
		if dryRun {
			stepLog.Info("change required (dry run, not applied)",
				"resource", action.Resource,
				"action", action.Kind.String(),
				"details", strings.Join(action.Details, "; "))
			continue
		}

		stepLog.Info("applying",
			"resource", action.Resource,
			"action", action.Kind.String(),
			"details", strings.Join(action.Details, "; "))
		if err := s.Apply(ctx, action); err != nil {
			stepLog.Error("apply failed", "resource", action.Resource, "error", err)
			return report, fmt.Errorf("applying %s: %w", s.Name(), err)
		}
		stepLog.Info("applied", "resource", action.Resource, "action", action.Kind.String())
	}

	report.IngestionURL = api.endpoint()
	logger.Info("converge complete",
		"steps", len(report.Actions),
		"changed", report.Changed(),
		"dry_run", dryRun,
		"ingestion_url", report.IngestionURL)
	return report, nil
}
