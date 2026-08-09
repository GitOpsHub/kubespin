package catalog

import (
	"context"
	"errors"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// fakeFileReader is an in-memory FileReader, standing in for a repo.Clients
// without needing GitHub credentials.
type fakeFileReader struct {
	files map[string][]byte // "repoName/path" -> content
}

func newFakeFileReader() *fakeFileReader { return &fakeFileReader{files: map[string][]byte{}} }

func (f *fakeFileReader) put(repoName, path string, content []byte) {
	f.files[repoName+"/"+path] = content
}

func (f *fakeFileReader) ReadFile(_ context.Context, repoName, path string) ([]byte, bool, error) {
	content, ok := f.files[repoName+"/"+path]
	return content, ok, nil
}

const testProfilesRepo = "platform-profiles"

func TestRepoResolver_Resolve(t *testing.T) {
	files := newFakeFileReader()
	files.put(testProfilesRepo, "profiles/tier-small/1.0.0.yaml", []byte(`
name: tier-small
version: 1.0.0
addons:
  - name: cert-manager
    chart: cert-manager
    repository: https://charts.jetstack.io
    version: 1.15.3
    namespace: cert-manager
`))
	r := NewRepoResolver(files, testProfilesRepo)

	profile, err := r.Resolve(context.Background(), core.ProfileRef{Name: "tier-small", Version: "1.0.0"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(profile.Addons) != 1 || profile.Addons[0].Name != "cert-manager" {
		t.Errorf("addons = %+v", profile.Addons)
	}
}

func TestRepoResolver_Resolve_NotFound(t *testing.T) {
	r := NewRepoResolver(newFakeFileReader(), testProfilesRepo)

	_, err := r.Resolve(context.Background(), core.ProfileRef{Name: "tier-small", Version: "9.9.9"})
	if !errors.Is(err, ErrProfileNotFound) {
		t.Errorf("error = %v, want one wrapping ErrProfileNotFound", err)
	}
}

func TestRepoResolver_Resolve_RejectsInvalidProfile(t *testing.T) {
	files := newFakeFileReader()
	files.put(testProfilesRepo, "profiles/tier-small/1.0.0.yaml", []byte(`
name: tier-small
version: 1.0.0
`)) // no addons — a profile must carry at least one
	r := NewRepoResolver(files, testProfilesRepo)

	_, err := r.Resolve(context.Background(), core.ProfileRef{Name: "tier-small", Version: "1.0.0"})
	if err == nil {
		t.Fatal("expected an error for a profile with no addons")
	}
}

func TestRepoResolver_Resolve_RejectsMismatchedRef(t *testing.T) {
	files := newFakeFileReader()
	// Filed under 1.0.0 but declares itself 2.0.0 — a copy-paste mistake in
	// the catalog repo that must not silently resolve as the requested version.
	files.put(testProfilesRepo, "profiles/tier-small/1.0.0.yaml", []byte(`
name: tier-small
version: 2.0.0
addons:
  - name: cert-manager
    chart: cert-manager
    repository: https://charts.jetstack.io
    version: 1.15.3
    namespace: cert-manager
`))
	r := NewRepoResolver(files, testProfilesRepo)

	_, err := r.Resolve(context.Background(), core.ProfileRef{Name: "tier-small", Version: "1.0.0"})
	if err == nil {
		t.Fatal("expected a mismatched profile ref to be rejected")
	}
}
