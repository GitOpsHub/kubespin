package argocd

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
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

	// crdInterval and crdTimeout bound the wait in restMapping. Fields rather
	// than constants only so tests need not sleep for real seconds.
	crdInterval time.Duration
	crdTimeout  time.Duration
}

// NewDynamicApplier builds a DynamicApplier.
func NewDynamicApplier(logger *slog.Logger) *DynamicApplier {
	if logger == nil {
		logger = slog.Default()
	}
	return &DynamicApplier{
		logger:      logger,
		crdInterval: crdEstablishInterval,
		crdTimeout:  crdEstablishTimeout,
	}
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

	mapping, err := a.restMapping(ctx, mapper, obj)
	if err != nil {
		return err
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

// crdEstablishInterval and crdEstablishTimeout bound the wait for a
// just-installed CRD to become usable.
const (
	crdEstablishInterval = 2 * time.Second
	crdEstablishTimeout  = 90 * time.Second
)

// restMapping resolves obj's kind to a REST resource, waiting for the kind to
// appear in discovery rather than failing the moment it is absent.
//
// This exists because of the order the Argo CD bootstrap necessarily runs in.
// HelmInstaller does not wait for its release to converge, so Install returns
// once the chart's manifests — including Argo CD's own CRDs — have been
// submitted, not once the API server has established them. The very next
// thing apply does is server-side-apply the root Application, an
// argoproj.io/v1alpha1 resource whose type only exists because that chart
// just created its CRD. Between those two moments the API server has to
// register the new type and discovery has to surface it, which takes seconds.
// Resolving the mapping once therefore failed with "no matches for kind
// Application" on fresh clusters — intermittently, which is the worst way for
// the last step of a half-hour provision to fail.
//
// Only a no-match is retried, and the cached discovery document is reset
// before each attempt: a stale cache is the actual reason the kind looks
// missing, so retrying without clearing it would spin until the timeout.
func (a *DynamicApplier) restMapping(
	ctx context.Context, mapper meta.ResettableRESTMapper, obj *unstructured.Unstructured,
) (*meta.RESTMapping, error) {
	gvk := obj.GroupVersionKind()

	interval, timeout := a.crdInterval, a.crdTimeout
	if interval <= 0 {
		interval = crdEstablishInterval
	}
	if timeout <= 0 {
		timeout = crdEstablishTimeout
	}
	deadline := time.Now().Add(timeout)

	for {
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err == nil {
			return mapping, nil
		}
		if !meta.IsNoMatchError(err) {
			return nil, fmt.Errorf("resolving REST mapping for %s %s/%s: %w",
				obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf(
				"resolving REST mapping for %s %s/%s: the cluster still does not serve %s after %s: %w",
				obj.GetKind(), obj.GetNamespace(), obj.GetName(), gvk.GroupVersion(), timeout, err)
		}

		a.logger.Info("waiting for the cluster to serve a just-installed resource type",
			"kind", obj.GetKind(), "group_version", gvk.GroupVersion().String())
		mapper.Reset()

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for %s to be served: %w", gvk.GroupVersion(), ctx.Err())
		case <-time.After(interval):
		}
	}
}

func boolPtr(b bool) *bool { return &b }
