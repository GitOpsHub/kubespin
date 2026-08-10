package argocd

import (
	"context"
	"fmt"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/yaml"
)

// fieldManager identifies kubespin's writes in a resource's managedFields, the
// same purpose a `kubectl apply --field-manager` value serves.
const fieldManager = "kubespin"

// KubeApplier applies a single manifest directly to a cluster's API server —
// used for exactly one resource in this codebase: app-of-apps' root
// Application, which is never committed to the repository it manages (see
// AppsDir's doc comment) and so cannot be delivered by Argo CD syncing itself.
type KubeApplier interface {
	// Apply server-side-applies manifest (a single YAML document) against the
	// cluster reachable via restConfig. It must be idempotent: applying the
	// same manifest twice converges rather than erroring or duplicating.
	Apply(ctx context.Context, restConfig *rest.Config, manifest []byte) error
}

// DynamicApplier is the real KubeApplier, built on client-go's dynamic client
// and discovery-backed REST mapper rather than shelling out to kubectl — the
// same discipline HelmInstaller follows for Argo CD's own install.
type DynamicApplier struct {
	logger *slog.Logger
}

// NewDynamicApplier builds a DynamicApplier.
func NewDynamicApplier(logger *slog.Logger) *DynamicApplier {
	if logger == nil {
		logger = slog.Default()
	}
	return &DynamicApplier{logger: logger}
}

// Apply implements KubeApplier via a server-side apply patch: re-applying an
// unchanged manifest is a no-op on the API server's side, which is what makes
// a resumed or repeated `apply` safe to call this on every time rather than
// needing its own existence check first.
func (a *DynamicApplier) Apply(ctx context.Context, restConfig *rest.Config, manifest []byte) error {
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(manifest, &obj.Object); err != nil {
		return fmt.Errorf("parsing manifest: %w", err)
	}

	dc, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("building discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))

	mapping, err := mapper.RESTMapping(obj.GroupVersionKind().GroupKind(), obj.GroupVersionKind().Version)
	if err != nil {
		return fmt.Errorf("resolving REST mapping for %s %s/%s: %w",
			obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
	}

	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("building dynamic client: %w", err)
	}

	resourceClient := dyn.Resource(mapping.Resource).Namespace(obj.GetNamespace())
	data, err := obj.MarshalJSON()
	if err != nil {
		return fmt.Errorf("encoding %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
	}

	_, err = resourceClient.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{
		FieldManager: fieldManager,
		Force:        boolPtr(true),
	})
	if err != nil {
		return fmt.Errorf("applying %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
	}

	a.logger.Info("applied manifest", "kind", obj.GetKind(), "namespace", obj.GetNamespace(), "name", obj.GetName())
	return nil
}

func boolPtr(b bool) *bool { return &b }
