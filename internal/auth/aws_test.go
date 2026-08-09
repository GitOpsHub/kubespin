package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// fakeSTS stands in for the real STS client so IsAuthenticated is testable
// without credentials, mirroring internal/fleetinfra's stsAPI fakes.
type fakeSTS struct {
	account string
	err     error
}

func (f *fakeSTS) GetCallerIdentity(
	context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options),
) (*sts.GetCallerIdentityOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &sts.GetCallerIdentityOutput{Account: aws.String(f.account)}, nil
}

// recordingRunner captures every shelled-out command instead of running it.
type recordingRunner struct {
	calls [][]string
	err   error
}

func (r *recordingRunner) run(_ context.Context, name string, args ...string) error {
	r.calls = append(r.calls, append([]string{name}, args...))
	return r.err
}

func TestAWSProvider_Name(t *testing.T) {
	p := &AWSProvider{profile: "default"}
	if p.Name() != "aws" {
		t.Errorf("Name() = %q, want aws", p.Name())
	}
}

func TestAWSProvider_IsAuthenticated_Valid(t *testing.T) {
	p := &AWSProvider{sts: &fakeSTS{account: "123456789012"}}

	ok, detail, err := p.IsAuthenticated(t.Context())
	if err != nil {
		t.Fatalf("IsAuthenticated: %v", err)
	}
	if !ok {
		t.Error("Authenticated = false, want true")
	}
	if !strings.Contains(detail.Message, "123456789012") {
		t.Errorf("detail = %+v, want the account number mentioned", detail)
	}
}

// An expired or missing session is reported as "not authenticated", not as
// an error — that distinction is what lets EnsureAll's message stay clean.
func TestAWSProvider_IsAuthenticated_Invalid(t *testing.T) {
	p := &AWSProvider{sts: &fakeSTS{err: errors.New("ExpiredTokenException")}}

	ok, _, err := p.IsAuthenticated(t.Context())
	if err != nil {
		t.Fatalf("IsAuthenticated returned an error: %v, want nil with ok=false", err)
	}
	if ok {
		t.Error("Authenticated = true, want false for an expired session")
	}
}

func TestAWSProvider_Login_RunsSSOLoginWithProfile(t *testing.T) {
	runner := &recordingRunner{}
	p := &AWSProvider{profile: "fleet", run: runner.run}

	if err := p.Login(t.Context()); err != nil {
		// aws CLI may genuinely be absent in CI — that is checkBinary working,
		// not a test bug, so only fail on something else going wrong.
		if !strings.Contains(err.Error(), "aws CLI not found") {
			t.Fatalf("Login: %v", err)
		}
		return
	}

	if len(runner.calls) != 1 {
		t.Fatalf("commands run = %v, want exactly one", runner.calls)
	}
	got := runner.calls[0]
	want := []string{"aws", "sso", "login", "--profile", "fleet"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("command = %v, want %v", got, want)
	}
}

func TestAWSProvider_Logout_RunsSSOLogout(t *testing.T) {
	runner := &recordingRunner{}
	p := &AWSProvider{profile: "default", run: runner.run}

	if err := p.Logout(t.Context()); err != nil {
		if !strings.Contains(err.Error(), "aws CLI not found") {
			t.Fatalf("Logout: %v", err)
		}
		return
	}

	if len(runner.calls) != 1 || strings.Join(runner.calls[0], " ") != "aws sso logout" {
		t.Errorf("commands run = %v, want [aws sso logout]", runner.calls)
	}
}

func TestCheckBinary_MissingBinaryReportsInstallHint(t *testing.T) {
	err := checkBinary("definitely-not-a-real-cli-xyz", "https://example.com/install")
	if err == nil {
		t.Fatal("expected an error for a binary that does not exist")
	}
	if !strings.Contains(err.Error(), "https://example.com/install") {
		t.Errorf("error %q does not mention the install hint", err)
	}
}

func TestNewAWSProvider_DefaultsProfile(t *testing.T) {
	p, err := NewAWSProvider(t.Context(), "")
	if err != nil {
		t.Fatalf("NewAWSProvider: %v", err)
	}
	if p.profile != "default" {
		t.Errorf("profile = %q, want default", p.profile)
	}
}
