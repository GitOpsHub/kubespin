package argocd

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestRenderRepoCredentialsSecret(t *testing.T) {
	rendered, err := RenderRepoCredentialsSecret("https://github.com/GitOpsHub/kubespin-demo.git", "x-access-token", "ghp_secrettoken")
	if err != nil {
		t.Fatalf("RenderRepoCredentialsSecret: %v", err)
	}

	var got map[string]any
	if err := yaml.Unmarshal(rendered, &got); err != nil {
		t.Fatalf("rendered manifest does not parse as YAML: %v\n%s", err, rendered)
	}

	if got["kind"] != "Secret" {
		t.Errorf("kind = %v, want Secret", got["kind"])
	}

	metadata, ok := got["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %+v, want a map", got["metadata"])
	}
	if metadata["namespace"] != Namespace {
		t.Errorf("namespace = %v, want %s", metadata["namespace"], Namespace)
	}
	labels, ok := metadata["labels"].(map[string]any)
	if !ok || labels["argocd.argoproj.io/secret-type"] != "repository" {
		// Without this label Argo CD's repo-server never picks the Secret up
		// as a repository credential at all — it just sits there inert.
		t.Errorf("labels = %+v, want argocd.argoproj.io/secret-type: repository", labels)
	}

	stringData, ok := got["stringData"].(map[string]any)
	if !ok {
		t.Fatalf("stringData = %+v, want a map", got["stringData"])
	}
	if stringData["type"] != "git" {
		t.Errorf("stringData.type = %v, want git", stringData["type"])
	}
	if stringData["url"] != "https://github.com/GitOpsHub/kubespin-demo.git" {
		t.Errorf("stringData.url = %v, want the repo's clone URL", stringData["url"])
	}
	if stringData["username"] != "x-access-token" {
		t.Errorf("stringData.username = %v, want x-access-token", stringData["username"])
	}
	if stringData["password"] != "ghp_secrettoken" {
		t.Errorf("stringData.password = %v, want the token passed in", stringData["password"])
	}

	// A leaked token in a log line is exactly the failure mode a secret
	// render function has to be paranoid about — the Marshal call is direct
	// struct-to-YAML, not string interpolation, specifically to avoid quoting
	// bugs that could smuggle a token's special characters somewhere unsafe.
	if !strings.Contains(string(rendered), "ghp_secrettoken") {
		t.Fatal("rendered manifest does not contain the password at all — something is wrong with the encoding")
	}
}
