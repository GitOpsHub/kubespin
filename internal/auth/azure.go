package auth

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const azureInstallHint = "https://learn.microsoft.com/cli/azure/install-azure-cli"

// azureManagementScope is the same scope internal/provisioner/azure's clients
// request implicitly through azidentity.NewDefaultAzureCredential — asking
// for it here is what makes IsAuthenticated a real proxy for whether those
// clients would succeed, not just whether `az` thinks it's logged in.
const azureManagementScope = "https://management.azure.com/.default"

// tokenCredential is the one call this package needs from azidentity's
// AzureCLICredential, narrowed for testability the same way stsAPI is.
type tokenCredential interface {
	GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error)
}

// AzureProvider authenticates via the az CLI: internal/provisioner/azure's
// NewDefaultAzureCredential falls back to exactly this session once no
// environment/managed-identity credential is available, so a logged-in az
// CLI is what makes kubespin's own Azure client construction work.
type AzureProvider struct {
	run     commandRunner
	newCred func() (tokenCredential, error)
}

// NewAzureProvider builds a provider over the az CLI's cached session.
func NewAzureProvider() *AzureProvider {
	return &AzureProvider{
		run: execRunner,
		newCred: func() (tokenCredential, error) {
			return azidentity.NewAzureCLICredential(nil)
		},
	}
}

// Name identifies this provider for --only and status output.
func (p *AzureProvider) Name() string { return "azure" }

// IsAuthenticated requests a real management-plane token rather than just
// checking `az account show`, so an expired or revoked session is reported
// accurately instead of a stale "yes" that fails moments later mid-apply.
func (p *AzureProvider) IsAuthenticated(ctx context.Context) (bool, StatusDetail, error) {
	cred, err := p.newCred()
	if err != nil {
		return false, StatusDetail{}, nil
	}

	tok, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{azureManagementScope}})
	if err != nil {
		return false, StatusDetail{}, nil
	}

	detail := StatusDetail{Message: "az CLI session valid"}
	if !tok.ExpiresOn.IsZero() {
		detail.ExpiresAt = &tok.ExpiresOn
	}
	return true, detail, nil
}

// Login shells out to `az login`, which handles the browser flow and caches
// the resulting session where azidentity.AzureCLICredential already knows to
// look.
func (p *AzureProvider) Login(ctx context.Context) error {
	if err := checkBinary("az", azureInstallHint); err != nil {
		return err
	}
	return p.run(ctx, "az", "login")
}

// Logout shells out to `az logout`, clearing the cached session.
func (p *AzureProvider) Logout(ctx context.Context) error {
	if err := checkBinary("az", azureInstallHint); err != nil {
		return err
	}
	return p.run(ctx, "az", "logout")
}
