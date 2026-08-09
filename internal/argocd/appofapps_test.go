package argocd

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/GitOpsHub/kubespin/internal/core"
)

func testProfile() core.Profile {
	return core.Profile{
		Name:    "tier-small",
		Version: "1.0.0",
		Addons: []core.AddonRef{
			{
				Name: "cert-manager", Chart: "cert-manager",
				Repository: "https://charts.jetstack.io", Version: "1.15.3", Namespace: "cert-manager",
				Values: map[string]any{"replicaCount": 1},
			},
			{
				Name: "external-dns", Chart: "external-dns",
				Repository: "https://kubernetes-sigs.github.io/external-dns", Version: "1.14.0", Namespace: "external-dns",
			},
		},
	}
}

func TestRenderRootApplication(t *testing.T) {
	rendered, err := RenderRootApplication("https://github.example.com/kubespin/kubespin-team-payments-prod")
	if err != nil {
		t.Fatalf("RenderRootApplication: %v", err)
	}

	var app Application
	if err := yaml.Unmarshal(rendered, &app); err != nil {
		t.Fatalf("rendered manifest does not parse as YAML: %v", err)
	}

	if app.Kind != "Application" || app.APIVersion != "argoproj.io/v1alpha1" {
		t.Errorf("apiVersion/kind = %s/%s", app.APIVersion, app.Kind)
	}
	if app.Spec.Source.Path != AppsDir {
		t.Errorf("source path = %q, want %q", app.Spec.Source.Path, AppsDir)
	}
	if app.Spec.Destination.Server != inClusterServer {
		t.Errorf("destination server = %q, want the in-cluster server", app.Spec.Destination.Server)
	}
	if app.Spec.SyncPolicy == nil || !app.Spec.SyncPolicy.Automated.Prune || !app.Spec.SyncPolicy.Automated.SelfHeal {
		t.Errorf("syncPolicy = %+v, want automated prune + selfHeal", app.Spec.SyncPolicy)
	}
}

func TestRenderAddonApplication(t *testing.T) {
	addon := testProfile().Addons[0]

	rendered, err := RenderAddonApplication(addon)
	if err != nil {
		t.Fatalf("RenderAddonApplication: %v", err)
	}

	var app Application
	if err := yaml.Unmarshal(rendered, &app); err != nil {
		t.Fatalf("rendered manifest does not parse as YAML: %v", err)
	}

	if app.Metadata.Name != addon.Name {
		t.Errorf("name = %q, want %q", app.Metadata.Name, addon.Name)
	}
	if app.Spec.Source.RepoURL != addon.Repository || app.Spec.Source.Chart != addon.Chart ||
		app.Spec.Source.TargetRevision != addon.Version {
		t.Errorf("source = %+v, does not match addon %+v", app.Spec.Source, addon)
	}
	if app.Spec.Destination.Namespace != addon.Namespace {
		t.Errorf("namespace = %q, want %q", app.Spec.Destination.Namespace, addon.Namespace)
	}
	if app.Spec.Source.Helm == nil || app.Spec.Source.Helm.ValuesObject["replicaCount"] != 1 {
		t.Errorf("helm values = %+v, want the addon's resolved values passed through", app.Spec.Source.Helm)
	}
}

func TestRenderAddonApplications_OneManifestPerAddon(t *testing.T) {
	profile := testProfile()

	rendered, err := RenderAddonApplications(profile)
	if err != nil {
		t.Fatalf("RenderAddonApplications: %v", err)
	}

	if len(rendered) != len(profile.Addons) {
		t.Fatalf("rendered %d manifests, want %d", len(rendered), len(profile.Addons))
	}
	for _, addon := range profile.Addons {
		path := AppsDir + "/" + addon.Name + ".yaml"
		content, ok := rendered[path]
		if !ok {
			t.Errorf("no manifest rendered at %s", path)
			continue
		}
		if !strings.Contains(string(content), "name: "+addon.Name) {
			t.Errorf("%s does not name its own addon: %s", path, content)
		}
	}
}

// Independent sync/failure is the entire point of app-of-apps: nothing about
// one addon's Application should reference another's.
func TestRenderAddonApplications_AddonsAreIndependent(t *testing.T) {
	rendered, err := RenderAddonApplications(testProfile())
	if err != nil {
		t.Fatalf("RenderAddonApplications: %v", err)
	}

	certManager := string(rendered[AppsDir+"/cert-manager.yaml"])
	if strings.Contains(certManager, "external-dns") {
		t.Error("cert-manager's Application references external-dns")
	}
}
