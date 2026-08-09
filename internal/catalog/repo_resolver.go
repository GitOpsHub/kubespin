package catalog

import (
	"context"
	"fmt"

	"go.yaml.in/yaml/v3"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// FileReader is the seam RepoResolver reads profile definitions through.
// *repo.Clients satisfies it; RepoResolver depends only on this interface so
// this package does not need to know anything about GitHub, or repo's fakes,
// to be tested.
type FileReader interface {
	ReadFile(ctx context.Context, repoName, path string) ([]byte, bool, error)
}

// RepoResolver resolves profiles from the platform-profiles repository: one
// YAML file per (name, version) pair, at profiles/<name>/<version>.yaml.
//
// This is the resolver the implementation plan's Milestone 4 describes.
// BuiltinResolver exists because this one needs a real GitHub org to point
// at; once that org has a platform-profiles repo seeded with real profile
// definitions, this is what production wires up instead.
type RepoResolver struct {
	files    FileReader
	repoName string
}

// NewRepoResolver builds a resolver reading profiles from repoName (typically
// "platform-profiles") through files.
func NewRepoResolver(files FileReader, repoName string) *RepoResolver {
	return &RepoResolver{files: files, repoName: repoName}
}

// Resolve reads and parses ref's profile definition.
func (r *RepoResolver) Resolve(ctx context.Context, ref core.ProfileRef) (core.Profile, error) {
	if err := ref.Validate(); err != nil {
		return core.Profile{}, fmt.Errorf("resolving %s: %w", ref, err)
	}

	path := fmt.Sprintf("profiles/%s/%s.yaml", ref.Name, ref.Version)
	content, ok, err := r.files.ReadFile(ctx, r.repoName, path)
	if err != nil {
		return core.Profile{}, fmt.Errorf("reading profile %s: %w", ref, err)
	}
	if !ok {
		return core.Profile{}, fmt.Errorf("%w: %s", ErrProfileNotFound, ref)
	}

	var profile core.Profile
	if err := yaml.Unmarshal(content, &profile); err != nil {
		return core.Profile{}, fmt.Errorf("parsing profile %s: %w", ref, err)
	}
	if err := profile.Validate(); err != nil {
		return core.Profile{}, fmt.Errorf("profile %s at %s: %w", ref, path, err)
	}
	if profile.Ref() != ref {
		return core.Profile{}, fmt.Errorf(
			"profile at %s declares itself %s, not the requested %s", path, profile.Ref(), ref)
	}

	return profile, nil
}
