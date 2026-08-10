package repo

import (
	"context"
	"fmt"
	"sync"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// Memory is an in-memory Provisioner.
//
// It exists so that every component built on Provisioner — the
// orchestrator above all — is testable without a GitHub token or a live
// organization. It honours the same commit-only-on-change contract the real
// GitHub-backed provisioner does: Push is a no-op, reporting no change, when
// every given file already matches what Clone read.
type Memory struct {
	mu       sync.Mutex
	repos    map[string]map[string][]byte // repo name -> path -> content
	archived map[string]bool
}

// NewMemory builds an in-memory Provisioner.
func NewMemory() *Memory {
	return &Memory{repos: map[string]map[string][]byte{}, archived: map[string]bool{}}
}

// Exists reports whether the cluster's repository has been created.
func (m *Memory) Exists(_ context.Context, spec core.ClusterSpec) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	_, ok := m.repos[names{spec}.repoName()]
	return ok, nil
}

// Create creates the repository and seeds CODEOWNERS. Idempotent.
func (m *Memory) Create(_ context.Context, spec core.ClusterSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	name := names{spec}.repoName()
	if _, ok := m.repos[name]; ok {
		return nil
	}
	m.repos[name] = map[string][]byte{"CODEOWNERS": []byte("* " + codeownersTeam + "\n")}
	return nil
}

// Clone reads the repository's tracked files.
func (m *Memory) Clone(_ context.Context, spec core.ClusterSpec) (*Checkout, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	name := names{spec}.repoName()
	files, ok := m.repos[name]
	if !ok {
		return nil, fmt.Errorf("repo: %s does not exist", name)
	}

	copied := make(map[string][]byte, len(files))
	for path, content := range files {
		copied[path] = content
	}
	return &Checkout{spec: spec, files: copied}, nil
}

// Push writes every changed file, reporting whether anything changed.
func (m *Memory) Push(_ context.Context, checkout *Checkout, files map[string][]byte, _ string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	name := names{checkout.spec}.repoName()
	store, ok := m.repos[name]
	if !ok {
		return false, fmt.Errorf("repo: %s does not exist", name)
	}

	changed := false
	for path, content := range files {
		if existing, ok := store[path]; !ok || string(existing) != string(content) {
			changed = true
		}
	}
	if !changed {
		return false, nil
	}

	for path, content := range files {
		store[path] = content
		checkout.files[path] = content
	}
	return true, nil
}

// RepoURL returns a stable, obviously-fake clone URL — Memory has no real
// GitHub host to point at, and nothing in this codebase parses this string,
// only renders it into a manifest a real Argo CD would clone from.
func (m *Memory) RepoURL(_ context.Context, spec core.ClusterSpec) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	name := names{spec}.repoName()
	if _, ok := m.repos[name]; !ok {
		return "", fmt.Errorf("repo: %s does not exist", name)
	}
	return "https://example.invalid/kubespin/" + name, nil
}

// Credentials returns an obviously-fake credential pair, matching RepoURL's
// obviously-fake host.
func (m *Memory) Credentials() (username, password string) {
	return "x-access-token", "fake-token"
}

// Archive marks the repository archived, or converges silently if it is
// already archived or was never created.
func (m *Memory) Archive(_ context.Context, spec core.ClusterSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	name := names{spec}.repoName()
	if _, ok := m.repos[name]; !ok {
		return nil
	}
	m.archived[name] = true
	return nil
}

// Archived reports whether spec's repository has been archived. Test-only
// visibility into state Archive itself does not expose, the way Memory's
// sibling packages' fakes expose their calls for assertions.
func (m *Memory) Archived(spec core.ClusterSpec) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.archived[names{spec}.repoName()]
}
