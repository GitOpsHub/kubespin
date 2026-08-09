package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// fakeTokenCredential stands in for azidentity.AzureCLICredential so
// IsAuthenticated is testable without a real az CLI session.
type fakeTokenCredential struct {
	token azcore.AccessToken
	err   error
}

func (f *fakeTokenCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return f.token, f.err
}

func TestAzureProvider_Name(t *testing.T) {
	if (&AzureProvider{}).Name() != "azure" {
		t.Errorf("Name() = %q, want azure", (&AzureProvider{}).Name())
	}
}

func TestAzureProvider_IsAuthenticated_Valid(t *testing.T) {
	expiry := time.Now().Add(time.Hour)
	p := &AzureProvider{
		newCred: func() (tokenCredential, error) {
			return &fakeTokenCredential{token: azcore.AccessToken{Token: "fake", ExpiresOn: expiry}}, nil
		},
	}

	ok, detail, err := p.IsAuthenticated(t.Context())
	if err != nil {
		t.Fatalf("IsAuthenticated: %v", err)
	}
	if !ok {
		t.Error("Authenticated = false, want true")
	}
	if detail.ExpiresAt == nil || !detail.ExpiresAt.Equal(expiry) {
		t.Errorf("ExpiresAt = %v, want %v", detail.ExpiresAt, expiry)
	}
}

func TestAzureProvider_IsAuthenticated_Invalid(t *testing.T) {
	p := &AzureProvider{
		newCred: func() (tokenCredential, error) {
			return &fakeTokenCredential{err: errors.New("no cached az CLI session")}, nil
		},
	}

	ok, _, err := p.IsAuthenticated(t.Context())
	if err != nil {
		t.Fatalf("IsAuthenticated returned an error: %v, want nil with ok=false", err)
	}
	if ok {
		t.Error("Authenticated = true, want false with no cached session")
	}
}

func TestAzureProvider_Login_RunsAzLogin(t *testing.T) {
	runner := &recordingRunner{}
	p := &AzureProvider{run: runner.run}

	if err := p.Login(t.Context()); err != nil {
		if !strings.Contains(err.Error(), "az CLI not found") {
			t.Fatalf("Login: %v", err)
		}
		return
	}

	if len(runner.calls) != 1 || strings.Join(runner.calls[0], " ") != "az login" {
		t.Errorf("commands run = %v, want [az login]", runner.calls)
	}
}

func TestAzureProvider_Logout_RunsAzLogout(t *testing.T) {
	runner := &recordingRunner{}
	p := &AzureProvider{run: runner.run}

	if err := p.Logout(t.Context()); err != nil {
		if !strings.Contains(err.Error(), "az CLI not found") {
			t.Fatalf("Logout: %v", err)
		}
		return
	}

	if len(runner.calls) != 1 || strings.Join(runner.calls[0], " ") != "az logout" {
		t.Errorf("commands run = %v, want [az logout]", runner.calls)
	}
}
