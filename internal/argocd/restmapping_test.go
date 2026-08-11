package argocd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// stubMapper reports the kind missing for the first `missing` calls, the way
// discovery does while a just-installed CRD is still being established, then
// resolves. It embeds meta.RESTMapper so only the two methods restMapping
// actually uses need implementing.
type stubMapper struct {
	meta.RESTMapper

	missing int
	err     error // returned instead of a no-match when set

	calls  int
	resets int
}

func (m *stubMapper) Reset() { m.resets++ }

func (m *stubMapper) RESTMapping(gk schema.GroupKind, _ ...string) (*meta.RESTMapping, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	if m.calls <= m.missing {
		return nil, &meta.NoKindMatchError{GroupKind: gk}
	}
	return &meta.RESTMapping{
		Resource: schema.GroupVersionResource{
			Group: gk.Group, Version: "v1alpha1", Resource: "applications",
		},
	}, nil
}

func testApplier() *DynamicApplier {
	return &DynamicApplier{
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		crdInterval: time.Millisecond,
		crdTimeout:  time.Second,
	}
}

func rootApplicationObject() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "argoproj.io", Version: "v1alpha1", Kind: "Application",
	})
	obj.SetNamespace(Namespace)
	obj.SetName("root")
	return obj
}

// TestRESTMapping_WaitsForAJustInstalledCRD is the regression test for the
// race between Helm submitting Argo CD's CRDs and the API server serving
// them: the root Application was applied immediately afterwards and failed
// with "no matches for kind Application" on fresh clusters.
func TestRESTMapping_WaitsForAJustInstalledCRD(t *testing.T) {
	mapper := &stubMapper{missing: 3}

	mapping, err := testApplier().restMapping(context.Background(), mapper, rootApplicationObject())
	if err != nil {
		t.Fatalf("restMapping: %v, want it to wait for the CRD", err)
	}
	if mapping.Resource.Resource != "applications" {
		t.Errorf("Resource = %q, want applications", mapping.Resource.Resource)
	}
	// The cached discovery document is the reason the kind looks missing, so
	// retrying without clearing it would spin until the timeout.
	if mapper.resets != 3 {
		t.Errorf("mapper reset %d times, want 3 — the discovery cache was not cleared between attempts", mapper.resets)
	}
}

// TestRESTMapping_ReturnsImmediatelyWhenServed keeps the retry from costing
// anything on the common path.
func TestRESTMapping_ReturnsImmediatelyWhenServed(t *testing.T) {
	mapper := &stubMapper{}

	if _, err := testApplier().restMapping(context.Background(), mapper, rootApplicationObject()); err != nil {
		t.Fatalf("restMapping: %v", err)
	}
	if mapper.calls != 1 || mapper.resets != 0 {
		t.Errorf("calls = %d, resets = %d; want 1 and 0", mapper.calls, mapper.resets)
	}
}

// TestRESTMapping_GivesUpWhenTheKindNeverAppears keeps the wait bounded, so a
// genuinely absent CRD fails with a clear message instead of hanging.
func TestRESTMapping_GivesUpWhenTheKindNeverAppears(t *testing.T) {
	mapper := &stubMapper{missing: 1_000_000}

	_, err := testApplier().restMapping(context.Background(), mapper, rootApplicationObject())
	if err == nil {
		t.Fatal("restMapping returned nil, want a timeout error")
	}
	if !strings.Contains(err.Error(), "still does not serve") {
		t.Errorf("error = %q, want it to say the cluster never served the type", err)
	}
}

// TestRESTMapping_DoesNotRetryOtherErrors keeps a real failure (an
// unreachable API server, an RBAC denial) from being hidden behind 90
// seconds of pointless retrying.
func TestRESTMapping_DoesNotRetryOtherErrors(t *testing.T) {
	boom := errors.New("connection refused")
	mapper := &stubMapper{err: boom}

	_, err := testApplier().restMapping(context.Background(), mapper, rootApplicationObject())
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap the underlying failure", err)
	}
	if mapper.calls != 1 {
		t.Errorf("mapper called %d times, want exactly 1 — a non-no-match error must not be retried", mapper.calls)
	}
}

// TestRESTMapping_StopsOnCancellation keeps an interrupted apply from sitting
// in this loop until the timeout.
func TestRESTMapping_StopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	mapper := &stubMapper{missing: 1_000_000}

	_, err := testApplier().restMapping(ctx, mapper, rootApplicationObject())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
