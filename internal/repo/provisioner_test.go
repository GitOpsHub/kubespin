package repo

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/go-github/v75/github"
)

func TestProvisioner_Exists(t *testing.T) {
	f := newFakeGitHub()
	p := NewProvisioner(f.clients())
	spec := testSpec()

	exists, err := p.Exists(context.Background(), spec)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Fatal("expected the repository not to exist yet")
	}

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	exists, err = p.Exists(context.Background(), spec)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Fatal("expected the repository to exist after Create")
	}
}

func TestProvisioner_Create_SeedsCodeownersAndProtection(t *testing.T) {
	f := newFakeGitHub()
	p := NewProvisioner(f.clients())
	spec := testSpec()

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	n := names{spec}
	if _, ok := f.protections[key(testOrg, n.repoName())+"/main"]; !ok {
		t.Error("expected branch protection to have been configured")
	}

	checkout, err := p.Clone(context.Background(), spec)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if content, ok := checkout.File("CODEOWNERS"); !ok || len(content) == 0 {
		t.Error("expected a seeded CODEOWNERS file")
	}
}

// A free-plan account cannot protect a private repository's branch. That is an
// account fact no retry fixes, so Create must still seed the repo rather than
// abandoning the apply partway through cluster provisioning.
func TestProvisioner_Create_ToleratesPlanWithoutBranchProtection(t *testing.T) {
	f := newFakeGitHub()
	f.protectionErr = &github.ErrorResponse{
		Response: &http.Response{StatusCode: http.StatusForbidden},
		Message:  "Upgrade to GitHub Pro or make this repository public to enable this feature.",
	}
	p := NewProvisioner(f.clients())
	spec := testSpec()

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create should converge without branch protection: %v", err)
	}

	checkout, err := p.Clone(context.Background(), spec)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if content, ok := checkout.File("CODEOWNERS"); !ok || len(content) == 0 {
		t.Error("expected a seeded CODEOWNERS file")
	}
}

// Every other 403 — a token missing admin scope, say — is a real
// misconfiguration and must fail loudly.
func TestProvisioner_Create_FailsOnForbiddenBranchProtection(t *testing.T) {
	f := newFakeGitHub()
	f.protectionErr = &github.ErrorResponse{
		Response: &http.Response{StatusCode: http.StatusForbidden},
		Message:  "Resource not accessible by personal access token",
	}
	p := NewProvisioner(f.clients())

	if err := p.Create(context.Background(), testSpec()); err == nil {
		t.Fatal("expected Create to fail when branch protection is forbidden for another reason")
	}
}

func TestProvisioner_Create_Idempotent(t *testing.T) {
	f := newFakeGitHub()
	p := NewProvisioner(f.clients())
	spec := testSpec()

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("second Create should converge, not fail: %v", err)
	}

	if got := countCalls(f, "Create"); got != 1 {
		t.Errorf("Create called %d times, want 1", got)
	}
}

func TestProvisioner_Push_NoChange_MakesNoCommit(t *testing.T) {
	f := newFakeGitHub()
	p := NewProvisioner(f.clients())
	spec := testSpec()

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	checkout, err := p.Clone(context.Background(), spec)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}

	existing, ok := checkout.File("CODEOWNERS")
	if !ok {
		t.Fatal("expected CODEOWNERS to already exist")
	}

	f.calls = nil // only the Push call below is under test

	changed, err := p.Push(context.Background(), checkout, map[string][]byte{"CODEOWNERS": existing}, "no-op")
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if changed {
		t.Error("expected pushing identical content to report no change")
	}
	f.assertNoMutations(t)
}

func TestProvisioner_Push_CommitsOnlyChangedFiles(t *testing.T) {
	f := newFakeGitHub()
	p := NewProvisioner(f.clients())
	spec := testSpec()

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	checkout, err := p.Clone(context.Background(), spec)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}

	changed, err := p.Push(context.Background(), checkout,
		map[string][]byte{ClusterFile: []byte("id: x\n")}, "add cluster.yaml")
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !changed {
		t.Fatal("expected a new file to be reported as a change")
	}

	reloaded, err := p.Clone(context.Background(), spec)
	if err != nil {
		t.Fatalf("Clone after push: %v", err)
	}
	if content, ok := reloaded.File(ClusterFile); !ok || string(content) != "id: x\n" {
		t.Errorf("cluster.yaml = %q, %v, want %q, true", content, ok, "id: x\n")
	}
	// The file seeded at Create must have survived: Push only ever adds to the
	// tree it read, it does not truncate it.
	if _, ok := reloaded.File("CODEOWNERS"); !ok {
		t.Error("expected CODEOWNERS to still exist after an unrelated push")
	}
}

func TestProvisioner_Archive(t *testing.T) {
	f := newFakeGitHub()
	p := NewProvisioner(f.clients())
	spec := testSpec()

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := p.Archive(context.Background(), spec); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	n := names{spec}
	repository, _, err := f.Get(context.Background(), testOrg, n.repoName())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !repository.GetArchived() {
		t.Error("expected the repository to be archived")
	}
}

func TestProvisioner_Archive_Idempotent(t *testing.T) {
	f := newFakeGitHub()
	p := NewProvisioner(f.clients())
	spec := testSpec()

	if err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := p.Archive(context.Background(), spec); err != nil {
		t.Fatalf("first Archive: %v", err)
	}

	editsBefore := countCalls(f, "Edit")

	if err := p.Archive(context.Background(), spec); err != nil {
		t.Fatalf("second Archive: %v", err)
	}
	if got := countCalls(f, "Edit"); got != editsBefore {
		t.Errorf("Edit called again on an already-archived repo: %d -> %d", editsBefore, got)
	}
}

func TestProvisioner_Archive_AbsentRepoConverges(t *testing.T) {
	f := newFakeGitHub()
	p := NewProvisioner(f.clients())

	if err := p.Archive(context.Background(), testSpec()); err != nil {
		t.Fatalf("Archive on an absent repo should converge, not fail: %v", err)
	}
}

func countCalls(f *fakeGitHub, name string) int {
	n := 0
	for _, c := range f.calls {
		if c == name {
			n++
		}
	}
	return n
}
