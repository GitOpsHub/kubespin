package repo

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-github/v75/github"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// fakeGitHub stands in for the Repositories and Git Data clients, recording
// every call by name so tests can assert which calls were made — which is
// how "no-change apply produces zero commits" is held to making no mutating
// calls at all.
//
// It models just enough of git to be faithful to how Push actually uses it: a
// tree is a full path->content snapshot (CreateTree merges its entries onto
// the base tree's snapshot, the way GitHub's real API does), a commit points
// at one tree, and a ref points at one commit. GetContents reads through a
// ref to its commit's tree, exactly as the real Contents API would.
type fakeGitHub struct {
	calls []string

	repos       map[string]*github.Repository
	protections map[string]*github.ProtectionRequest
	// protectionErr, when set, is what UpdateBranchProtection returns instead
	// of succeeding — how tests reproduce GitHub refusing branch protection.
	protectionErr error
	refs          map[string]string            // "org/repo/heads/branch" -> commit sha
	commits       map[string]*github.Commit    // sha -> commit
	trees         map[string]map[string]string // sha -> path -> content
	seq           int
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		repos:       map[string]*github.Repository{},
		protections: map[string]*github.ProtectionRequest{},
		refs:        map[string]string{},
		commits:     map[string]*github.Commit{},
		trees:       map[string]map[string]string{},
	}
}

func (f *fakeGitHub) record(name string) { f.calls = append(f.calls, name) }

var mutatingCalls = []string{
	"Create", "UpdateBranchProtection", "CreateTree", "CreateCommit", "UpdateRef", "Edit",
}

func (f *fakeGitHub) assertNoMutations(t *testing.T) {
	t.Helper()
	for _, call := range f.calls {
		for _, mutator := range mutatingCalls {
			if call == mutator {
				t.Errorf("expected no repo changes, but %s was called", call)
			}
		}
	}
}

func (f *fakeGitHub) clients() *Clients { return &Clients{org: testOrg, repo: f, git: f} }

func (f *fakeGitHub) genSHA() string {
	f.seq++
	return fmt.Sprintf("sha%d", f.seq)
}

func key(owner, repo string) string { return owner + "/" + repo }

func ok200() *github.Response { return &github.Response{Response: &http.Response{StatusCode: 200}} }
func resp404() *github.Response {
	return &github.Response{Response: &http.Response{StatusCode: 404}}
}

// --- Repositories ---

func (f *fakeGitHub) Get(_ context.Context, owner, repoName string) (*github.Repository, *github.Response, error) {
	f.record("Get")
	r, ok := f.repos[key(owner, repoName)]
	if !ok {
		return nil, resp404(), fmt.Errorf("404")
	}
	return r, ok200(), nil
}

func (f *fakeGitHub) Create(_ context.Context, org string, in *github.Repository) (*github.Repository, *github.Response, error) {
	f.record("Create")
	name := in.GetName()
	if _, ok := f.repos[key(org, name)]; ok {
		return nil, nil, fmt.Errorf("repository already exists")
	}

	created := &github.Repository{Name: github.Ptr(name), DefaultBranch: github.Ptr("main")}
	f.repos[key(org, name)] = created

	if in.GetAutoInit() {
		treeSHA := f.genSHA()
		f.trees[treeSHA] = map[string]string{"README.md": "# " + name}
		commitSHA := f.genSHA()
		f.commits[commitSHA] = &github.Commit{SHA: github.Ptr(commitSHA), Tree: &github.Tree{SHA: github.Ptr(treeSHA)}}
		f.refs[key(org, name)+"/heads/main"] = commitSHA
	}
	return created, ok200(), nil
}

func (f *fakeGitHub) Edit(
	_ context.Context, owner, repoName string, patch *github.Repository,
) (*github.Repository, *github.Response, error) {
	f.record("Edit")
	r, ok := f.repos[key(owner, repoName)]
	if !ok {
		return nil, resp404(), fmt.Errorf("404")
	}
	if patch.Archived != nil {
		r.Archived = patch.Archived
	}
	return r, ok200(), nil
}

func (f *fakeGitHub) GetContents(
	_ context.Context, owner, repoName, path string, opts *github.RepositoryContentGetOptions,
) (*github.RepositoryContent, []*github.RepositoryContent, *github.Response, error) {
	f.record("GetContents")

	branch := "main"
	if opts != nil && opts.Ref != "" {
		branch = opts.Ref
	}
	commitSHA, ok := f.refs[key(owner, repoName)+"/heads/"+branch]
	if !ok {
		return nil, nil, resp404(), fmt.Errorf("404")
	}
	tree := f.trees[f.commits[commitSHA].GetTree().GetSHA()]

	if content, ok := tree[path]; ok {
		return &github.RepositoryContent{
			Content:  github.Ptr(base64.StdEncoding.EncodeToString([]byte(content))),
			Encoding: github.Ptr("base64"),
		}, nil, ok200(), nil
	}

	// path did not match a file exactly; treat it as a directory listing, the
	// same fallback the real Contents API offers.
	prefix := path + "/"
	var dirContents []*github.RepositoryContent
	for p := range tree {
		if rest, ok := strings.CutPrefix(p, prefix); ok && !strings.Contains(rest, "/") {
			dirContents = append(dirContents, &github.RepositoryContent{
				Path: github.Ptr(p), Type: github.Ptr("file"),
			})
		}
	}
	if len(dirContents) > 0 {
		return nil, dirContents, ok200(), nil
	}
	return nil, nil, resp404(), fmt.Errorf("404")
}

func (f *fakeGitHub) UpdateBranchProtection(
	_ context.Context, owner, repoName, branch string, preq *github.ProtectionRequest,
) (*github.Protection, *github.Response, error) {
	f.record("UpdateBranchProtection")
	if f.protectionErr != nil {
		return nil, nil, f.protectionErr
	}
	f.protections[key(owner, repoName)+"/"+branch] = preq
	return &github.Protection{}, ok200(), nil
}

// --- Git Data ---

func (f *fakeGitHub) GetRef(_ context.Context, owner, repoName, ref string) (*github.Reference, *github.Response, error) {
	f.record("GetRef")
	sha, ok := f.refs[key(owner, repoName)+"/"+ref]
	if !ok {
		return nil, resp404(), fmt.Errorf("404")
	}
	return &github.Reference{Ref: github.Ptr("refs/" + ref), Object: &github.GitObject{SHA: github.Ptr(sha)}}, ok200(), nil
}

func (f *fakeGitHub) GetCommit(_ context.Context, _, _, sha string) (*github.Commit, *github.Response, error) {
	f.record("GetCommit")
	c, ok := f.commits[sha]
	if !ok {
		return nil, resp404(), fmt.Errorf("404")
	}
	return c, ok200(), nil
}

func (f *fakeGitHub) CreateTree(
	_ context.Context, _, _, baseTree string, entries []*github.TreeEntry,
) (*github.Tree, *github.Response, error) {
	f.record("CreateTree")

	merged := map[string]string{}
	for k, v := range f.trees[baseTree] {
		merged[k] = v
	}
	for _, e := range entries {
		merged[e.GetPath()] = e.GetContent()
	}

	sha := f.genSHA()
	f.trees[sha] = merged
	return &github.Tree{SHA: github.Ptr(sha)}, ok200(), nil
}

func (f *fakeGitHub) CreateCommit(
	_ context.Context, _, _ string, commit github.Commit, _ *github.CreateCommitOptions,
) (*github.Commit, *github.Response, error) {
	f.record("CreateCommit")
	sha := f.genSHA()
	commit.SHA = github.Ptr(sha)
	f.commits[sha] = &commit
	return &commit, ok200(), nil
}

func (f *fakeGitHub) UpdateRef(
	_ context.Context, owner, repoName, ref string, updateRef github.UpdateRef,
) (*github.Reference, *github.Response, error) {
	f.record("UpdateRef")
	f.refs[key(owner, repoName)+"/"+ref] = updateRef.SHA
	return &github.Reference{Ref: github.Ptr("refs/" + ref), Object: &github.GitObject{SHA: github.Ptr(updateRef.SHA)}}, ok200(), nil
}

// --- helpers ---

const testOrg = "kubespin-test-org"

func testSpec() core.ClusterSpec {
	return core.ClusterSpec{
		ID:       "team-payments-prod",
		Provider: core.ProviderAWS,
		Region:   "us-east-1",
		Access:   core.AccessPrivate,
		Subnets:  []string{"subnet-aaa", "subnet-bbb"},
		NodePools: []core.NodePool{{
			Name: "default", InstanceType: "m6i.large", MinSize: 1, MaxSize: 5, DesiredSize: 3,
		}},
		Profile: core.ProfileRef{Name: "tier-small", Version: "1.0.0"},
	}
}

func testProfile() core.Profile {
	return core.Profile{
		Name:    "tier-small",
		Version: "1.0.0",
		Addons: []core.AddonRef{{
			Name: "cert-manager", Chart: "cert-manager",
			Repository: "https://charts.jetstack.io", Version: "1.15.3", Namespace: "cert-manager",
		}},
	}
}
