package argocd

import (
	"fmt"

	"go.yaml.in/yaml/v3"
)

// repoCredsSecretName is fixed, not derived from the repository: a cluster
// has exactly one Argo CD instance managing exactly one repository (itself),
// so there is nothing per-repository to key the name on, and a stable name
// is what makes re-applying it on every apply converge rather than
// accumulating one Secret per run.
const repoCredsSecretName = "repo-creds" //nolint:gosec // a Secret's name, not a credential

// repoCredsSecret is the subset of a core/v1 Secret this package writes, the
// same local-mirror-of-the-CRD approach Application takes for the Argo CD
// CRD: kubespin only ever writes these fields, never reads them back.
type repoCredsSecret struct {
	APIVersion string              `yaml:"apiVersion"`
	Kind       string              `yaml:"kind"`
	Metadata   repoCredsMetadata   `yaml:"metadata"`
	Type       string              `yaml:"type"`
	StringData repoCredsStringData `yaml:"stringData"`
}

type repoCredsMetadata struct {
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace"`
	Labels    map[string]string `yaml:"labels"`
}

type repoCredsStringData struct {
	Type     string `yaml:"type"`
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// RenderRepoCredentialsSecret renders the repository-credential Secret Argo
// CD's repo-server needs to clone the cluster's own repository.
//
// The repository is always created private (internal/repo.Provisioner.Create
// never passes Public: true), so without this Secret, the root Application
// RenderRootApplication points at fails its very first reconcile with
// "authentication required" and never discovers a single addon underneath
// AppsDir — this is what makes that failure mode possible in the first
// place, and applying this Secret alongside the root Application is what
// closes it.
//
// Applied directly to the cluster the same way the root Application is
// (KubeApplier.Apply), never committed to the repository it grants access
// to: a Secret holding a live token has no business living in git history.
func RenderRepoCredentialsSecret(repoURL, username, password string) ([]byte, error) {
	secret := repoCredsSecret{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: repoCredsMetadata{
			Name:      repoCredsSecretName,
			Namespace: Namespace,
			Labels:    map[string]string{"argocd.argoproj.io/secret-type": "repository"},
		},
		Type: "Opaque",
		StringData: repoCredsStringData{
			Type: "git", URL: repoURL, Username: username, Password: password,
		},
	}

	rendered, err := yaml.Marshal(secret)
	if err != nil {
		return nil, fmt.Errorf("rendering repository credentials Secret: %w", err)
	}
	return rendered, nil
}
