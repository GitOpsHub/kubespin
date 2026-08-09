package auth

import (
	"context"
	"fmt"
)

const gcpInstallHint = "https://cloud.google.com/sdk/docs/install"

// GCPProvider authenticates via gcloud: a user login (for interactive/CLI
// use) plus Application Default Credentials (what the GCP SDK clients in
// internal/provisioner/gcp actually read).
type GCPProvider struct {
	run commandRunner
	out commandOutput
}

// NewGCPProvider builds a provider that shells out to the gcloud CLI.
func NewGCPProvider() *GCPProvider {
	return &GCPProvider{run: execRunner, out: execOutput}
}

// Name identifies this provider for --only and status output.
func (p *GCPProvider) Name() string { return "gcp" }

// IsAuthenticated asks gcloud to actually mint an access token from the
// cached Application Default Credentials, rather than just checking that a
// credentials file exists — a revoked or expired token fails this the same
// way it would fail a real GCP SDK call, which is the point.
func (p *GCPProvider) IsAuthenticated(ctx context.Context) (bool, StatusDetail, error) {
	if _, err := p.out(ctx, "gcloud", "auth", "application-default", "print-access-token"); err != nil {
		return false, StatusDetail{}, nil
	}

	detail := StatusDetail{Message: "application default credentials valid"}
	if account, err := p.out(ctx, "gcloud", "config", "get-value", "account"); err == nil && account != "" && account != "(unset)" {
		detail.Message = fmt.Sprintf("logged in as %s", account)
	}
	return true, detail, nil
}

// Login runs the two logins gcloud distinguishes: a user login (what
// `gcloud` itself uses) and an Application Default Credentials login (what
// every GCP client library, including internal/provisioner/gcp's, reads).
// Skipping the second is the classic "gcloud works but my Go program can't
// authenticate" trap.
func (p *GCPProvider) Login(ctx context.Context) error {
	if err := checkBinary("gcloud", gcpInstallHint); err != nil {
		return err
	}
	if err := p.run(ctx, "gcloud", "auth", "login"); err != nil {
		return err
	}
	return p.run(ctx, "gcloud", "auth", "application-default", "login")
}

// Logout revokes both the user session and the Application Default
// Credentials, mirroring the two logins above.
func (p *GCPProvider) Logout(ctx context.Context) error {
	if err := checkBinary("gcloud", gcpInstallHint); err != nil {
		return err
	}
	if err := p.run(ctx, "gcloud", "auth", "application-default", "revoke", "--quiet"); err != nil {
		return err
	}
	return p.run(ctx, "gcloud", "auth", "revoke", "--all", "--quiet")
}
