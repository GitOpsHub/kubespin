package repo

import (
	"context"
	"testing"

	"github.com/google/go-github/v75/github"
)

// seedFakeRepo creates repoName (outside the per-cluster "kubespin-" naming
// scheme, the way platform-profiles is) and commits files directly through
// the fake's Git Data primitives, bypassing Provisioner entirely: ReadFile is
// not scoped to a cluster's own repository, so nothing here should go through
// Checkout, which is.
func seedFakeRepo(t *testing.T, f *fakeGitHub, repoName string, files map[string]string) {
	t.Helper()
	ctx := context.Background()

	if _, _, err := f.Create(ctx, testOrg, &github.Repository{
		Name: github.Ptr(repoName), AutoInit: github.Ptr(true),
	}); err != nil {
		t.Fatalf("seeding fake repo %s: %v", repoName, err)
	}

	ref, _, err := f.GetRef(ctx, testOrg, repoName, "heads/main")
	if err != nil {
		t.Fatalf("GetRef: %v", err)
	}
	baseCommit, _, err := f.GetCommit(ctx, testOrg, repoName, ref.GetObject().GetSHA())
	if err != nil {
		t.Fatalf("GetCommit: %v", err)
	}

	var entries []*github.TreeEntry
	for path, content := range files {
		entries = append(entries, &github.TreeEntry{
			Path: github.Ptr(path), Mode: github.Ptr("100644"), Type: github.Ptr("blob"),
			Content: github.Ptr(content),
		})
	}
	tree, _, err := f.CreateTree(ctx, testOrg, repoName, baseCommit.GetTree().GetSHA(), entries)
	if err != nil {
		t.Fatalf("CreateTree: %v", err)
	}
	commit, _, err := f.CreateCommit(ctx, testOrg, repoName, github.Commit{
		Message: github.Ptr("seed"), Tree: tree,
		Parents: []*github.Commit{{SHA: github.Ptr(ref.GetObject().GetSHA())}},
	}, nil)
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	if _, _, err := f.UpdateRef(ctx, testOrg, repoName, "heads/main", github.UpdateRef{SHA: commit.GetSHA()}); err != nil {
		t.Fatalf("UpdateRef: %v", err)
	}
}

func TestClients_ReadFile(t *testing.T) {
	f := newFakeGitHub()
	seedFakeRepo(t, f, "platform-profiles", map[string]string{
		"profiles/tier-small/1.0.0.yaml": "name: tier-small\nversion: 1.0.0\n",
	})

	content, ok, err := f.clients().ReadFile(context.Background(), "platform-profiles", "profiles/tier-small/1.0.0.yaml")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !ok {
		t.Fatal("expected the file to exist")
	}
	if string(content) != "name: tier-small\nversion: 1.0.0\n" {
		t.Errorf("content = %q", content)
	}
}

func TestClients_ReadFile_NotFound(t *testing.T) {
	f := newFakeGitHub()
	seedFakeRepo(t, f, "platform-profiles", nil)

	_, ok, err := f.clients().ReadFile(context.Background(), "platform-profiles", "profiles/does-not-exist.yaml")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if ok {
		t.Fatal("expected the file not to exist")
	}
}
