package repo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/go-github/v75/github"

	"github.com/GitOpsHub/kubespin/internal/argocd"
	"github.com/GitOpsHub/kubespin/internal/core"
)

// Checkout is a snapshot of a cluster repo's tracked files, read through the
// GitHub Contents API rather than a literal git clone: this package only ever
// needs cluster.yaml, addons.yaml, and .state.yaml, not the repository's full
// history or working tree.
type Checkout struct {
	spec   core.ClusterSpec
	branch string
	// baseCommitSHA is the branch tip Checkout was read from. Push builds its
	// new commit on top of it, and UpdateRef fails (rather than silently
	// discarding a concurrent change) if the branch has moved since.
	baseCommitSHA string
	files         map[string][]byte // path -> content, only for files that exist
}

// File returns a tracked file's content, and whether it exists.
func (c *Checkout) File(path string) ([]byte, bool) {
	content, ok := c.files[path]
	return content, ok
}

// Provisioner manages a cluster's GitHub repository.
type Provisioner interface {
	// Exists reports whether the cluster's repository has been created.
	Exists(ctx context.Context, spec core.ClusterSpec) (bool, error)

	// Create creates the repository, protects its default branch, and seeds a
	// CODEOWNERS file. It is idempotent: creating an existing repo is a no-op.
	// Branch protection is best-effort — an account plan that does not offer
	// it on private repositories leaves the branch unprotected with a warning
	// rather than failing.
	Create(ctx context.Context, spec core.ClusterSpec) error

	// Clone reads the repository's tracked files off its default branch.
	Clone(ctx context.Context, spec core.ClusterSpec) (*Checkout, error)

	// Push commits every file in files that differs from what checkout read,
	// as one atomic commit, and updates the default branch to point at it. It
	// reports whether it made a commit: a files argument that already matches
	// checkout is a no-op, which is what makes a no-change `apply` produce
	// zero commits.
	Push(ctx context.Context, checkout *Checkout, files map[string][]byte, message string) (bool, error)

	// Archive archives the repository. Used by teardown: a decommissioned
	// cluster's repo is archived, never deleted, so its history survives. It
	// is idempotent — archiving an already-archived or absent repository is a
	// no-op — so a retried teardown converges rather than failing.
	Archive(ctx context.Context, spec core.ClusterSpec) error

	// RepoURL returns the repository's clone URL — what the app-of-apps root
	// Application (internal/argocd.RenderRootApplication) points Argo CD's own
	// repo-server at, so it must be a URL Argo CD's repo-server can clone, not
	// merely an identifier.
	RepoURL(ctx context.Context, spec core.ClusterSpec) (string, error)
}

// githubProvisioner is the real Provisioner, backed by GitHub's REST and
// Git Data APIs.
type githubProvisioner struct {
	c      *Clients
	logger *slog.Logger
}

// Option configures a Provisioner.
type Option func(*githubProvisioner)

// WithLogger sets the logger. Without it, a provisioner logs to
// slog.Default().
func WithLogger(logger *slog.Logger) Option {
	return func(p *githubProvisioner) {
		if logger != nil {
			p.logger = logger
		}
	}
}

// NewProvisioner builds a Provisioner over the given clients.
func NewProvisioner(c *Clients, opts ...Option) Provisioner {
	p := &githubProvisioner{c: c, logger: slog.Default()}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

var (
	_ Provisioner = (*githubProvisioner)(nil)
	_ Provisioner = (*Memory)(nil)
)

// Exists reports whether the cluster's repository has been created.
func (p *githubProvisioner) Exists(ctx context.Context, spec core.ClusterSpec) (bool, error) {
	n := names{spec}

	_, resp, err := p.c.repo.Get(ctx, p.c.org, n.repoName())
	if err != nil {
		if notFound(resp) {
			return false, nil
		}
		return false, fmt.Errorf("checking whether %s exists: %w", n.repoName(), err)
	}
	return true, nil
}

// Create creates the repository, protects its default branch (requiring
// CODEOWNERS review), and seeds CODEOWNERS. It is idempotent: an existing
// repository or an already-seeded CODEOWNERS file is left alone. Branch
// protection is best-effort: see protectBranch.
//
// AutoInit is deliberate: it gives the new repository an initial commit on
// its default branch, so Clone/Push have a ref to build on rather than
// needing a special case for a completely empty repository.
func (p *githubProvisioner) Create(ctx context.Context, spec core.ClusterSpec) error {
	n := names{spec}

	exists, err := p.Exists(ctx, spec)
	if err != nil {
		return err
	}

	var branch string
	if !exists {
		created, _, err := p.c.repo.Create(ctx, p.c.org, &github.Repository{
			Name:     github.Ptr(n.repoName()),
			Private:  github.Ptr(true),
			AutoInit: github.Ptr(true),
			Description: github.Ptr(fmt.Sprintf(
				"kubespin-managed cluster repository for %s", spec.ID)),
		})
		if err != nil {
			return fmt.Errorf("creating repository %s: %w", n.repoName(), err)
		}
		branch = created.GetDefaultBranch()
		p.logger.Info("created cluster repository",
			"cluster", spec.ID, "repo", n.repoName(), "branch", branch)
	} else {
		repository, _, err := p.c.repo.Get(ctx, p.c.org, n.repoName())
		if err != nil {
			return fmt.Errorf("reading repository %s: %w", n.repoName(), err)
		}
		branch = repository.GetDefaultBranch()
		p.logger.Info("cluster repository already exists",
			"cluster", spec.ID, "repo", n.repoName(), "branch", branch)
	}

	if err := p.protectBranch(ctx, n, branch); err != nil {
		return err
	}
	return p.seedCodeowners(ctx, spec, branch)
}

func (p *githubProvisioner) protectBranch(ctx context.Context, n names, branch string) error {
	_, _, err := p.c.repo.UpdateBranchProtection(ctx, p.c.org, n.repoName(), branch, &github.ProtectionRequest{
		RequiredPullRequestReviews: &github.PullRequestReviewsEnforcementRequest{
			RequireCodeOwnerReviews:      true,
			RequiredApprovingReviewCount: 1,
		},
		EnforceAdmins: true,
	})
	switch {
	case err == nil:
		return nil
	case planLacksBranchProtection(err):
		// The account's plan does not offer branch protection on a private
		// repository. That is an account fact, not a kubespin misconfiguration
		// or a transient failure, and retrying or failing the whole apply over
		// it would leave the cluster half-provisioned for something no retry
		// can fix. Converge without protection and say so loudly instead.
		p.logger.Warn("branch protection unavailable on this GitHub plan; repository left unprotected",
			"repo", n.repoName(), "branch", branch, "error", err)
		return nil
	default:
		return fmt.Errorf("protecting %s branch %s: %w", n.repoName(), branch, err)
	}
}

// planLacksBranchProtection reports whether GitHub refused branch protection
// because of the account's plan rather than the request. github.com answers
// 403 "Upgrade to GitHub Pro or make this repository public to enable this
// feature." for a private repo on a free plan. The message match is
// deliberate: a 403 from a token missing admin scope is a real
// misconfiguration and must still fail.
func planLacksBranchProtection(err error) bool {
	var errResp *github.ErrorResponse
	if !errors.As(err, &errResp) {
		return false
	}
	if errResp.Response == nil || errResp.Response.StatusCode != http.StatusForbidden {
		return false
	}
	return strings.Contains(errResp.Message, "Upgrade to GitHub")
}

// seedCodeowners writes CODEOWNERS directly through the Contents-backed
// Checkout/Push path rather than a special-cased call, so it goes through the
// same atomic-commit machinery as every other file this package writes.
func (p *githubProvisioner) seedCodeowners(ctx context.Context, spec core.ClusterSpec, branch string) error {
	checkout, err := p.cloneBranch(ctx, spec, branch)
	if err != nil {
		return err
	}
	if _, ok := checkout.File("CODEOWNERS"); ok {
		return nil
	}

	_, err = p.Push(ctx, checkout,
		map[string][]byte{"CODEOWNERS": []byte("* " + codeownersTeam + "\n")},
		"kubespin: seed CODEOWNERS")
	return err
}

// Clone reads cluster.yaml, addons.yaml, and .state.yaml off the repository's
// default branch.
func (p *githubProvisioner) Clone(ctx context.Context, spec core.ClusterSpec) (*Checkout, error) {
	n := names{spec}

	repository, _, err := p.c.repo.Get(ctx, p.c.org, n.repoName())
	if err != nil {
		return nil, fmt.Errorf("reading repository %s: %w", n.repoName(), err)
	}
	return p.cloneBranch(ctx, spec, repository.GetDefaultBranch())
}

// RepoURL returns the repository's HTTPS clone URL.
func (p *githubProvisioner) RepoURL(ctx context.Context, spec core.ClusterSpec) (string, error) {
	n := names{spec}

	repository, _, err := p.c.repo.Get(ctx, p.c.org, n.repoName())
	if err != nil {
		return "", fmt.Errorf("reading repository %s: %w", n.repoName(), err)
	}
	return repository.GetCloneURL(), nil
}

func (p *githubProvisioner) cloneBranch(ctx context.Context, spec core.ClusterSpec, branch string) (*Checkout, error) {
	n := names{spec}

	ref, _, err := p.c.git.GetRef(ctx, p.c.org, n.repoName(), "heads/"+branch)
	if err != nil {
		return nil, fmt.Errorf("reading %s branch %s: %w", n.repoName(), branch, err)
	}

	checkout := &Checkout{
		spec: spec, branch: branch,
		baseCommitSHA: ref.GetObject().GetSHA(),
		files:         map[string][]byte{},
	}

	for _, path := range []string{ClusterFile, AddonsFile, StateFile, "CODEOWNERS"} {
		content, _, resp, err := p.c.repo.GetContents(ctx, p.c.org, n.repoName(), path,
			&github.RepositoryContentGetOptions{Ref: branch})
		if err != nil {
			if notFound(resp) {
				continue
			}
			return nil, fmt.Errorf("reading %s from %s: %w", path, n.repoName(), err)
		}
		decoded, err := content.GetContent()
		if err != nil {
			return nil, fmt.Errorf("decoding %s from %s: %w", path, n.repoName(), err)
		}
		checkout.files[path] = []byte(decoded)
	}

	appPaths, err := p.listAppsDir(ctx, n, branch)
	if err != nil {
		return nil, err
	}
	for _, path := range appPaths {
		content, _, resp, err := p.c.repo.GetContents(ctx, p.c.org, n.repoName(), path,
			&github.RepositoryContentGetOptions{Ref: branch})
		if err != nil {
			if notFound(resp) {
				continue
			}
			return nil, fmt.Errorf("reading %s from %s: %w", path, n.repoName(), err)
		}
		decoded, err := content.GetContent()
		if err != nil {
			return nil, fmt.Errorf("decoding %s from %s: %w", path, n.repoName(), err)
		}
		checkout.files[path] = []byte(decoded)
	}

	return checkout, nil
}

// listAppsDir lists the app-of-apps directory's current files, so cloneBranch
// can track them in Checkout the same way it tracks the three fixed files
// above. Without this, every addon Application under argocd.AppsDir would
// look "new" on every Push — checkout would never have seen it — and a
// no-change apply would recommit the whole directory instead of making no
// commit at all.
func (p *githubProvisioner) listAppsDir(ctx context.Context, n names, branch string) ([]string, error) {
	_, dirContents, resp, err := p.c.repo.GetContents(ctx, p.c.org, n.repoName(), argocd.AppsDir,
		&github.RepositoryContentGetOptions{Ref: branch})
	if err != nil {
		if notFound(resp) {
			// No apps/ directory yet — the first apply that installs Argo CD
			// creates it.
			return nil, nil
		}
		return nil, fmt.Errorf("listing %s in %s: %w", argocd.AppsDir, n.repoName(), err)
	}

	paths := make([]string, 0, len(dirContents))
	for _, entry := range dirContents {
		if entry.GetType() == "file" {
			paths = append(paths, entry.GetPath())
		}
	}
	return paths, nil
}

// Push commits every file in files that differs from checkout, as one atomic
// commit, and advances the default branch to point at it.
//
// A files argument that already matches checkout makes no API calls beyond
// the diff itself: that is what makes a no-change `apply` produce zero
// commits, the invariant Milestone 3's acceptance criteria is built on.
func (p *githubProvisioner) Push(
	ctx context.Context, checkout *Checkout, files map[string][]byte, message string,
) (bool, error) {
	n := names{checkout.spec}

	entries := changedEntries(checkout, files)
	if len(entries) == 0 {
		return false, nil
	}

	baseCommit, _, err := p.c.git.GetCommit(ctx, p.c.org, n.repoName(), checkout.baseCommitSHA)
	if err != nil {
		return false, fmt.Errorf("reading base commit for %s: %w", n.repoName(), err)
	}

	tree, _, err := p.c.git.CreateTree(ctx, p.c.org, n.repoName(), baseCommit.GetTree().GetSHA(), entries)
	if err != nil {
		return false, fmt.Errorf("building tree for %s: %w", n.repoName(), err)
	}

	commit, _, err := p.c.git.CreateCommit(ctx, p.c.org, n.repoName(), github.Commit{
		Message: github.Ptr(message),
		Tree:    tree,
		Parents: []*github.Commit{{SHA: github.Ptr(checkout.baseCommitSHA)}},
	}, nil)
	if err != nil {
		return false, fmt.Errorf("committing to %s: %w", n.repoName(), err)
	}

	_, _, err = p.c.git.UpdateRef(ctx, p.c.org, n.repoName(), "heads/"+checkout.branch,
		github.UpdateRef{SHA: commit.GetSHA()})
	if err != nil {
		return false, fmt.Errorf("advancing %s branch %s: %w", n.repoName(), checkout.branch, err)
	}

	p.logger.Info("pushed commit to cluster repository",
		"cluster", checkout.spec.ID, "repo", n.repoName(), "branch", checkout.branch,
		"commit", commit.GetSHA(), "files", len(entries), "message", message)

	checkout.baseCommitSHA = commit.GetSHA()
	for path, content := range files {
		checkout.files[path] = content
	}
	return true, nil
}

// Archive archives the repository, or converges silently if it is already
// archived or was never created — the same "already there" tolerance every
// other Create/Delete-shaped method in this codebase gives a retried run.
func (p *githubProvisioner) Archive(ctx context.Context, spec core.ClusterSpec) error {
	n := names{spec}

	repository, resp, err := p.c.repo.Get(ctx, p.c.org, n.repoName())
	if err != nil {
		if notFound(resp) {
			return nil
		}
		return fmt.Errorf("reading repository %s: %w", n.repoName(), err)
	}
	if repository.GetArchived() {
		return nil
	}

	if _, _, err := p.c.repo.Edit(ctx, p.c.org, n.repoName(), &github.Repository{
		Archived: github.Ptr(true),
	}); err != nil {
		return fmt.Errorf("archiving repository %s: %w", n.repoName(), err)
	}

	p.logger.Info("archived cluster repository", "cluster", spec.ID, "repo", n.repoName())
	return nil
}

// changedEntries returns tree entries for exactly the files that differ from
// checkout, so an unchanged file is never rewritten into the commit.
func changedEntries(checkout *Checkout, files map[string][]byte) []*github.TreeEntry {
	var entries []*github.TreeEntry
	for path, content := range files {
		if existing, ok := checkout.File(path); ok && string(existing) == string(content) {
			continue
		}
		entries = append(entries, &github.TreeEntry{
			Path:    github.Ptr(path),
			Mode:    github.Ptr("100644"),
			Type:    github.Ptr("blob"),
			Content: github.Ptr(string(content)),
		})
	}
	return entries
}
