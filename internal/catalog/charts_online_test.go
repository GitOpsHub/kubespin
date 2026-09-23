package catalog

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/GitOpsHub/kubespin/internal/argocd"
	"github.com/GitOpsHub/kubespin/internal/core"
)

// TestCatalogCharts_Exist checks, against the live chart repositories, that
// every chart version the catalog pins can actually be fetched. A pin that
// does not resolve fails only once Argo CD tries it on a real cluster —
// OpenCost pinned a version its repository never published, and four
// addons pointed at a repository that does not exist — so this is what to
// run before shipping a catalog change.
//
// It needs the network (and helm, for OCI charts), so it only runs when
// KUBESPIN_CHECK_CHARTS is set: `make check-charts`.
func TestCatalogCharts_Exist(t *testing.T) {
	if os.Getenv("KUBESPIN_CHECK_CHARTS") == "" {
		t.Skip("set KUBESPIN_CHECK_CHARTS=1 (make check-charts) to check chart pins against their repositories")
	}

	addons := []core.AddonRef{argocd.DefaultAddon}
	for _, size := range []core.Profile{sizeSmall, sizeMedium, sizeLarge} {
		addons = append(addons, size.Addons...)
	}
	checked := map[string]bool{}
	indexes := map[string]map[string][]string{}
	for _, a := range addons {
		key := a.Repository + " " + a.Chart + " " + a.Version
		if checked[key] {
			continue
		}
		checked[key] = true
		t.Run(a.Name+"@"+a.Version, func(t *testing.T) {
			if err := chartExists(t.Context(), a, indexes); err != nil {
				t.Error(err)
			}
		})
	}
}

func chartExists(ctx context.Context, a core.AddonRef, indexes map[string]map[string][]string) error {
	if strings.HasPrefix(a.Repository, "oci://") {
		if _, err := exec.LookPath("helm"); err != nil {
			return fmt.Errorf("helm is needed to check OCI chart %s: %w", a.Repository, err)
		}
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, "helm", "show", "chart", a.Repository, "--version", a.Version).CombinedOutput() //nolint:gosec // catalog-owned values
		if err != nil {
			return fmt.Errorf("%s %s: %s", a.Repository, a.Version, strings.TrimSpace(string(out)))
		}
		return nil
	}

	index, ok := indexes[a.Repository]
	if !ok {
		var err error
		if index, err = fetchIndex(ctx, a.Repository); err != nil {
			return err
		}
		indexes[a.Repository] = index
	}
	versions, ok := index[a.Chart]
	if !ok {
		return fmt.Errorf("repository %s has no chart %s", a.Repository, a.Chart)
	}
	if !slices.Contains(versions, a.Version) {
		return fmt.Errorf("repository %s has no %s version %s", a.Repository, a.Chart, a.Version)
	}
	return nil
}

func fetchIndex(ctx context.Context, repo string) (map[string][]string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(repo, "/")+"/index.yaml", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", repo, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: HTTP %d", repo, resp.StatusCode)
	}

	var index struct {
		Entries map[string][]struct {
			Version string `yaml:"version"`
		} `yaml:"entries"`
	}
	if err := yaml.NewDecoder(resp.Body).Decode(&index); err != nil {
		return nil, fmt.Errorf("parsing %s index: %w", repo, err)
	}
	out := make(map[string][]string, len(index.Entries))
	for chart, entries := range index.Entries {
		for _, e := range entries {
			out[chart] = append(out[chart], strings.TrimPrefix(e.Version, "v"))
		}
	}
	return out, nil
}
