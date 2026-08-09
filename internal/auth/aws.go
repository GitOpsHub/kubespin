package auth

import (
	"context"
	"fmt"

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
}

// NewAWSProvider builds a provider scoped to one named profile in
// ~/.aws/config. It succeeds even before the operator has ever logged in —
// LoadDefaultConfig only fails on a malformed config, not on missing
// credentials — so a fresh checkout can still run `kubespin login`.
func NewAWSProvider(ctx context.Context, profile string) (*AWSProvider, error) {
	if profile == "" {
		profile = "default"
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithSharedConfigProfile(profile))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config for profile %s: %w", profile, err)
	}

	return &AWSProvider{
		profile: profile,
		sts:     sts.NewFromConfig(cfg),
		run:     execRunner,
	}, nil
}

// Name identifies this provider for --only and status output.
func (p *AWSProvider) Name() string { return "aws" }

// IsAuthenticated calls GetCallerIdentity rather than just checking for a
// cached token file, so an expired or revoked session is reported accurately
// instead of a stale "yes" that fails moments later mid-apply.
func (p *AWSProvider) IsAuthenticated(ctx context.Context) (bool, StatusDetail, error) {
	out, err := p.sts.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
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
	return p.run(ctx, "aws", "sso", "login", "--profile", p.profile)
}

// Logout shells out to `aws sso logout`, clearing the cached SSO token.
func (p *AWSProvider) Logout(ctx context.Context) error {
	if err := checkBinary("aws", awsInstallHint); err != nil {
		return err
	}
	return p.run(ctx, "aws", "sso", "logout")
}
