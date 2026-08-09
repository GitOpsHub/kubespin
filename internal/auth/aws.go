package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const awsInstallHint = "https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html"

// stsAPI is the one call this package needs, narrowed the same way
// internal/fleetinfra's client interfaces are: it is what makes
// IsAuthenticated testable without real AWS credentials.
type stsAPI interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// AWSProvider authenticates via AWS IAM Identity Center (SSO): the same flow
// documented in docs/fleet-bootstrap.md for the Fleet Registry account.
type AWSProvider struct {
	profile string
	sts     stsAPI
	run     commandRunner
	logger  *slog.Logger
}

// NewAWSProvider builds a provider scoped to one named profile in
// ~/.aws/config. It succeeds even before the operator has ever logged in or
// run `aws configure` at all — a profile forced via WithSharedConfigProfile
// fails if that profile's section doesn't exist yet, so a missing "default"
// section (a fresh checkout, a bare CI runner) falls back to the SDK's
// unscoped default resolution instead of erroring; a named profile that
// doesn't exist still errors, since the operator explicitly asked for it.
func NewAWSProvider(ctx context.Context, profile string, opts ...Option) (*AWSProvider, error) {
	o := resolve(opts)

	if profile == "" {
		profile = "default"
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithSharedConfigProfile(profile))
	if err != nil {
		var notExist config.SharedConfigProfileNotExistError
		if profile == "default" && errors.As(err, &notExist) {
			cfg, err = config.LoadDefaultConfig(ctx)
		}
		if err != nil {
			return nil, fmt.Errorf("loading AWS config for profile %s: %w", profile, err)
		}
	}

	return &AWSProvider{
		profile: profile,
		sts:     sts.NewFromConfig(cfg),
		run:     execRunner,
		logger:  o.logger,
	}, nil
}

// Name identifies this provider for --only and status output.
func (p *AWSProvider) Name() string { return "aws" }

func (p *AWSProvider) log() *slog.Logger { return loggerOr(p.logger) }

// IsAuthenticated calls GetCallerIdentity rather than just checking for a
// cached token file, so an expired or revoked session is reported accurately
// instead of a stale "yes" that fails moments later mid-apply.
func (p *AWSProvider) IsAuthenticated(ctx context.Context) (bool, StatusDetail, error) {
	p.log().Debug("checking aws session", "provider", "aws", "profile", p.profile)

	out, err := p.sts.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		p.log().Debug("aws session is not usable",
			"provider", "aws", "profile", p.profile, "error", err)
		// Not authenticated is the overwhelmingly common reason this call
		// fails (expired SSO token, never logged in), and distinguishing it
		// from a transient network error isn't something the SDK error gives
		// us cleanly — so this is reported as "not authenticated", not as an
		// error, consistent with the Provider.IsAuthenticated contract.
		return false, StatusDetail{}, nil
	}
	return true, StatusDetail{
		Message: fmt.Sprintf("account %s reachable", aws.ToString(out.Account)),
	}, nil
}

// Login shells out to `aws sso login`, which handles the browser flow and
// caches the resulting token where the AWS SDK's default credential chain
// already knows to look (~/.aws/sso/cache).
func (p *AWSProvider) Login(ctx context.Context) error {
	if err := checkBinary("aws", awsInstallHint); err != nil {
		return err
	}
	p.log().Debug("shelling out to aws sso login", "provider", "aws", "profile", p.profile)
	return p.run(ctx, "aws", "sso", "login", "--profile", p.profile)
}

// Logout shells out to `aws sso logout`, clearing the cached SSO token.
func (p *AWSProvider) Logout(ctx context.Context) error {
	if err := checkBinary("aws", awsInstallHint); err != nil {
		return err
	}
	p.log().Debug("shelling out to aws sso logout", "provider", "aws", "profile", p.profile)
	return p.run(ctx, "aws", "sso", "logout")
}
