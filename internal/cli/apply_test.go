package cli

import (
	"testing"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// A GCP or Azure run must not require AWS credentials. The registry used to
// be DynamoDB, which made AWS auth universal; once it became operator-supplied
// Postgres, demanding AWS only produced "not authenticated to aws" on machines
// that were never going to talk to AWS at all.
func TestCloudAuthProviders_OnlyTheClustersOwnCloud(t *testing.T) {
	for _, provider := range []core.Provider{core.ProviderAWS, core.ProviderGCP, core.ProviderAzure} {
		got := cloudAuthProviders(core.ClusterSpec{Provider: provider})
		want := []string{string(provider)}
		if len(got) != 1 || got[0] != want[0] {
			t.Errorf("cloudAuthProviders(%s) = %v, want %v", provider, got, want)
		}
	}
}
