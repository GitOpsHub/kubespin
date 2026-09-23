package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/GitOpsHub/kubespin/internal/argocd"
	"github.com/GitOpsHub/kubespin/internal/core"
)

// Addon Applications self-heal, so draining while Argo CD's application
// controller still runs lets it recreate every deleted LoadBalancer Service —
// and the cloud a fresh load balancer the cluster's deletion then orphans.
// The controller must be stopped before the first Service is deleted.
func TestDrainLoadBalancers_StopsArgoCDBeforeDeletingServices(t *testing.T) {
	replicas := int32(1)
	clientset := fake.NewClientset(
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "argocd-application-controller", Namespace: argocd.Namespace,
				Labels: map[string]string{"app.kubernetes.io/component": "application-controller"},
			},
			Spec: appsv1.StatefulSetSpec{Replicas: &replicas},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "web-ui", Namespace: "web-ui"},
			Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "internal", Namespace: "default"},
			Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP},
		},
	)

	spec := core.ClusterSpec{ID: "team-alpha"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := drainLoadBalancersWith(context.Background(), clientset, spec, logger); err != nil {
		t.Fatalf("drainLoadBalancersWith: %v", err)
	}

	scaledAt, deletedAt := -1, -1
	for i, action := range clientset.Actions() {
		switch {
		case action.Matches("update", "statefulsets") && scaledAt < 0:
			scaledAt = i
		case action.Matches("delete", "services") && deletedAt < 0:
			deletedAt = i
		}
	}
	if scaledAt < 0 || deletedAt < 0 || scaledAt > deletedAt {
		t.Fatalf("want the application controller scaled before any Service delete; scale at %d, delete at %d", scaledAt, deletedAt)
	}

	sts, err := clientset.AppsV1().StatefulSets(argocd.Namespace).Get(context.Background(), "argocd-application-controller", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting controller: %v", err)
	}
	if got := *sts.Spec.Replicas; got != 0 {
		t.Errorf("controller replicas = %d, want 0", got)
	}
	if _, err := clientset.CoreV1().Services("default").Get(context.Background(), "internal", metav1.GetOptions{}); err != nil {
		t.Errorf("ClusterIP Service was touched: %v", err)
	}
}

// A cluster that never got Argo CD (or already lost it) still drains.
func TestDrainLoadBalancers_WithoutArgoCD(t *testing.T) {
	clientset := fake.NewClientset(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "lb", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := drainLoadBalancersWith(context.Background(), clientset, core.ClusterSpec{ID: "team-alpha"}, logger); err != nil {
		t.Fatalf("drainLoadBalancersWith: %v", err)
	}
	if _, err := clientset.CoreV1().Services("default").Get(context.Background(), "lb", metav1.GetOptions{}); err == nil {
		t.Error("LoadBalancer Service survived the drain")
	}
}
