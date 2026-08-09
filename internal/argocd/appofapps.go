package argocd

import (
	"fmt"

	"go.yaml.in/yaml/v3"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// AppsDir is where per-addon Application manifests are committed in a
// cluster's own repository. RenderRootApplication points at it; a resumed or
// repeated apply re-renders the same files, so committing them goes through
// the same repo.Push change-detection every other file in the repo does.
const AppsDir = "apps"

// RenderRootApplication renders the app-of-apps root Application: the one
// resource installed directly into the cluster (never committed to the repo
// it manages — an Application that synced itself would be a cycle), which
// discovers every manifest under AppsDir in the cluster's own repository.
func RenderRootApplication(repoURL string) ([]byte, error) {
	app := Application{
		APIVersion: "argoproj.io/v1alpha1",
		Kind:       "Application",
		Metadata: ApplicationMetadata{
			Name: "root", Namespace: Namespace,
			Finalizers: []string{"resources-finalizer.argocd.argoproj.io"},
		},
		Spec: ApplicationSpec{
			Project: "default",
			Source: ApplicationSource{
				RepoURL: repoURL, Path: AppsDir, TargetRevision: "HEAD",
			},
			Destination: ApplicationDestination{Server: inClusterServer, Namespace: Namespace},
			SyncPolicy: &ApplicationSyncPolicy{
				Automated: &ApplicationSyncPolicyAutomated{Prune: true, SelfHeal: true},
			},
		},
	}

	rendered, err := yaml.Marshal(app)
	if err != nil {
		return nil, fmt.Errorf("rendering root Application: %w", err)
	}
	return rendered, nil
}

// RenderAddonApplication renders one addon's independent Application: each
// addon syncs and fails on its own, which is the entire point of app-of-apps
// over one monolithic Application for the whole profile.
func RenderAddonApplication(addon core.AddonRef) ([]byte, error) {
	app := Application{
		APIVersion: "argoproj.io/v1alpha1",
		Kind:       "Application",
		Metadata: ApplicationMetadata{
			Name: addon.Name, Namespace: Namespace,
			Finalizers: []string{"resources-finalizer.argocd.argoproj.io"},
		},
		Spec: ApplicationSpec{
			Project: "default",
			Source: ApplicationSource{
				RepoURL: addon.Repository, Chart: addon.Chart, TargetRevision: addon.Version,
				Helm: &ApplicationSourceHelm{ValuesObject: addon.Values},
			},
			Destination: ApplicationDestination{Server: inClusterServer, Namespace: addon.Namespace},
			SyncPolicy: &ApplicationSyncPolicy{
				Automated:   &ApplicationSyncPolicyAutomated{Prune: true, SelfHeal: true},
				SyncOptions: []string{"CreateNamespace=true"},
			},
		},
	}

	rendered, err := yaml.Marshal(app)
	if err != nil {
		return nil, fmt.Errorf("rendering Application for addon %s: %w", addon.Name, err)
	}
	return rendered, nil
}

// RenderAddonApplications renders every addon in profile, keyed by the path
// each should be committed to under AppsDir in the cluster's own repository.
func RenderAddonApplications(profile core.Profile) (map[string][]byte, error) {
	out := make(map[string][]byte, len(profile.Addons))
	for _, addon := range profile.Addons {
		rendered, err := RenderAddonApplication(addon)
		if err != nil {
			return nil, err
		}
		out[AppsDir+"/"+addon.Name+".yaml"] = rendered
	}
	return out, nil
}
