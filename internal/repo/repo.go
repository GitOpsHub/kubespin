// Package repo manages each cluster's GitHub repository: the cluster.yaml,
// addons.yaml, and .state.yaml contract described in the project's CLAUDE.md.
//
// It reaches GitHub entirely through the REST and Git Data APIs
// (github.com/google/go-github), never a literal `git clone`/`git push`. That
// keeps a "clone" to the three files this package cares about rather than
// the repository's full history, and lets every multi-file update land as one
// atomic commit through the Git Data API (create tree -> create commit ->
// update ref) instead of one REST call per file.
package repo

import (
	"context"
	"fmt"

	"github.com/google/go-github/v75/github"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// File paths inside every cluster repository. Their roles must stay distinct:
// see the "Cluster repo contract" section of the project's CLAUDE.md.
const (
	ClusterFile = "cluster.yaml"
	AddonsFile  = "addons.yaml"
	StateFile   = ".state.yaml"
)

// repositoriesAPI is the go-github Repositories surface this package uses.
type repositoriesAPI interface {
	Get(ctx context.Context, owner, repo string) (*github.Repository, *github.Response, error)
	Create(ctx context.Context, org string, repo *github.Repository) (*github.Repository, *github.Response, error)
	GetContents(
		ctx context.Context, owner, repo, path string, opts *github.RepositoryContentGetOptions,
	) (*github.RepositoryContent, []*github.RepositoryContent, *github.Response, error)
	UpdateBranchProtection(
		ctx context.Context, owner, repo, branch string, preq *github.ProtectionRequest,
	) (*github.Protection, *github.Response, error)
	Edit(ctx context.Context, owner, repo string, repository *github.Repository) (*github.Repository, *github.Response, error)
}

// gitAPI is the go-github Git Data surface this package uses to build one
// atomic commit out of several changed files.
type gitAPI interface {
	GetRef(ctx context.Context, owner, repo, ref string) (*github.Reference, *github.Response, error)
	GetCommit(ctx context.Context, owner, repo, sha string) (*github.Commit, *github.Response, error)
	CreateTree(ctx context.Context, owner, repo, baseTree string, entries []*github.TreeEntry) (*github.Tree, *github.Response, error)
	CreateCommit(
		ctx context.Context, owner, repo string, commit github.Commit, opts *github.CreateCommitOptions,
	) (*github.Commit, *github.Response, error)
	UpdateRef(ctx context.Context, owner, repo, ref string, updateRef github.UpdateRef) (*github.Reference, *github.Response, error)
}

// Clients bundles the GitHub clients this package uses, scoped to one
// organization. The organization is fixed at construction, the way AWS's
// Clients fixes a region: it is operator configuration, not cluster desired
// state.
type Clients struct {
	org   string
	repo  repositoriesAPI
	git   gitAPI
	token string
}

// NewClients builds a real GitHub client. baseURL and uploadURL configure a
// GitHub Enterprise instance; leave both empty for github.com.
func NewClients(org, baseURL, uploadURL, token string) (*Clients, error) {
	if org == "" {
		return nil, fmt.Errorf("repo: org is required")
	}
	if token == "" {
		return nil, fmt.Errorf("repo: token is required")
	}

	client := github.NewClient(nil).WithAuthToken(token)
	if baseURL != "" {
		var err error
		client, err = client.WithEnterpriseURLs(baseURL, uploadURL)
		if err != nil {
			return nil, fmt.Errorf("configuring GitHub Enterprise URLs: %w", err)
		}
	}

	return &Clients{org: org, repo: client.Repositories, git: client.Git, token: token}, nil
}

// names derives every GitHub resource name from the cluster ID, so a
// cluster's repository is identifiable and a second cluster cannot collide
// with it.
type names struct {
	spec core.ClusterSpec
}

func (n names) repoName() string { return "kubespin-" + n.spec.ID.String() }

// codeownersTeam is the CODEOWNERS entry seeded into every cluster repo.
// M4 or later can make this per-cluster; a single platform team is the
// correct default until there is a reason to vary it.
const codeownersTeam = "@GitOpsHub/platform-team"

func notFound(resp *github.Response) bool {
	return resp != nil && resp.StatusCode == 404
}
