package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/GitOpsHub/kubespin/internal/argocd"
	"github.com/GitOpsHub/kubespin/internal/core"
)

// hashApps combines every rendered app-of-apps file into one hash, order
// -independent of map iteration so the same addon set always hashes the same
// way regardless of Go's randomised map order.
func hashApps(apps map[string][]byte) string {
	paths := make([]string, 0, len(apps))
	for path := range apps {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	h := sha256.New()
	for _, path := range paths {
		h.Write([]byte(path))
		h.Write([]byte{0})
		h.Write(apps[path])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ReconcileAppOfApps brings a cluster's app-of-apps addon Applications (under
// argocd.AppsDir) in line with its resolved profile, the same idempotent-diff
// discipline ReconcileAddons applies to addons.yaml: it commits only when the
// rendered set differs from what .state.yaml last recorded, so a no-change
// apply makes no commit here either.
//
// The root Application is applied straight to the cluster
// (internal/argocd.KubeApplier); a copy is committed at RootApplicationFile,
// outside argocd.AppsDir, so the repository holds every piece of the
// cluster's Argo CD configuration without the root ever syncing itself. The
// repo-creds Secret is the one piece deliberately never committed: it
// carries the operator's GitHub token.
func ReconcileAppOfApps(ctx context.Context, rp Provisioner, spec core.ClusterSpec, profile core.Profile) (bool, error) {
	apps, err := argocd.RenderAddonApplications(profile)
	if err != nil {
		return false, fmt.Errorf("rendering app-of-apps for %s: %w", spec.ID, err)
	}

	repoURL, err := rp.RepoURL(ctx, spec)
	if err != nil {
		return false, fmt.Errorf("resolving repository URL for %s: %w", spec.ID, err)
	}
	rootApp, err := argocd.RenderRootApplication(repoURL)
	if err != nil {
		return false, fmt.Errorf("rendering root Application for %s: %w", spec.ID, err)
	}

	desired := make(map[string][]byte, len(apps)+1)
	for path, content := range apps {
		desired[path] = content
	}
	desired[RootApplicationFile] = rootApp
	desiredHash := hashApps(desired)

	checkout, err := rp.Clone(ctx, spec)
	if err != nil {
		return false, fmt.Errorf("reading repository for %s: %w", spec.ID, err)
	}

	currentState, err := loadState(checkout)
	if err != nil {
		return false, fmt.Errorf("%s: %w", spec.ID, err)
	}
	if currentState.AppsHash == desiredHash {
		return false, nil
	}
	currentState.AppsHash = desiredHash

	stateYAML, err := yaml.Marshal(currentState)
	if err != nil {
		return false, fmt.Errorf("rendering %s: %w", StateFile, err)
	}

	files := make(map[string][]byte, len(desired)+1)
	for path, content := range desired {
		files[path] = content
	}
	// Mark any apps that existed in the repository but are absent from the
	// newly resolved addon set as deleted (nil content).
	for path := range checkout.files {
		if strings.HasPrefix(path, argocd.AppsDir+"/") {
			if _, ok := apps[path]; !ok {
				files[path] = nil
			}
		}
	}
	files[StateFile] = stateYAML

	committed, err := rp.Push(ctx, checkout, files, "kubespin: sync app-of-apps Applications")
	if err != nil {
		return false, fmt.Errorf("pushing app-of-apps for %s: %w", spec.ID, err)
	}
	return committed, nil
}
