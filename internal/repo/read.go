package repo

import (
	"context"
	"fmt"

	"github.com/google/go-github/v75/github"
)

// ReadFile reads one file off a repository's default branch in this
// package's organization.
//
// Unlike Provisioner, this is not scoped to a cluster's own repository: it is
// what internal/catalog's repo-backed Resolver uses to read profile
// definitions out of the platform-profiles repository, which is a different
// repository in the same org. It reports ok=false rather than an error when
// the file does not exist, the same convention Checkout.File uses, so a
// missing profile version is an ordinary "not found" rather than a surprise.
func (c *Clients) ReadFile(ctx context.Context, repoName, path string) ([]byte, bool, error) {
	repository, _, err := c.repo.Get(ctx, c.org, repoName)
	if err != nil {
		return nil, false, fmt.Errorf("reading repository %s: %w", repoName, err)
	}

	content, _, resp, err := c.repo.GetContents(ctx, c.org, repoName, path,
		&github.RepositoryContentGetOptions{Ref: repository.GetDefaultBranch()})
	if err != nil {
		if notFound(resp) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading %s from %s: %w", path, repoName, err)
	}

	decoded, err := content.GetContent()
	if err != nil {
		return nil, false, fmt.Errorf("decoding %s from %s: %w", path, repoName, err)
	}
	return []byte(decoded), true, nil
}
