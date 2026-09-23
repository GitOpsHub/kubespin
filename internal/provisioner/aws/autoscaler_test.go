package aws

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/GitOpsHub/kubespin/internal/core"
)

func TestReconcile_BindsTheClusterAutoscalerIdentity(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	f.activeCluster(spec)
	f.withNodePool(spec, spec.NodePools[0])

	if _, err := NewClusterProvisioner(f.clients()).Reconcile(t.Context(), spec); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, ok := f.addons[addonPodIdentityAgent]; !ok {
		t.Errorf("addon %s was not installed", addonPodIdentityAgent)
	}
	if f.addons[addonPodIdentityAgent] != nil && f.addons[addonPodIdentityAgent].ServiceAccountRoleArn != nil {
		t.Error("the Pod Identity agent was given a service account role; EKS rejects an empty one")
	}

	role := names{spec}.clusterAutoscalerRole()
	if !strings.Contains(f.rolePolicy[role], "pods.eks.amazonaws.com") {
		t.Errorf("trust policy for %s does not trust EKS Pod Identity: %s", role, f.rolePolicy[role])
	}

	encoded, ok := f.inlinePolicies[role][clusterAutoscalerPolicyName]
	if !ok {
		t.Fatalf("role %s has no %s inline policy", role, clusterAutoscalerPolicyName)
	}
	doc, _ := url.QueryUnescape(encoded)
	// Resizing must stay scoped to this cluster's own Auto Scaling groups.
	if !strings.Contains(doc, "aws:ResourceTag/k8s.io/cluster-autoscaler/"+spec.ID.String()) {
		t.Errorf("policy does not scope writes to this cluster's node groups: %s", doc)
	}
	if !json.Valid([]byte(doc)) {
		t.Errorf("policy is not valid JSON: %s", doc)
	}

	var a ekstypes.PodIdentityAssociation
	for _, pi := range f.podIdentities {
		if aws.ToString(pi.ServiceAccount) == core.ClusterAutoscalerServiceAccount {
			a = pi
		}
	}
	if aws.ToString(a.Namespace) != core.ClusterAutoscalerNamespace ||
		aws.ToString(a.ServiceAccount) != core.ClusterAutoscalerServiceAccount ||
		aws.ToString(a.RoleArn) != f.roles[role] {
		t.Errorf("association = %s/%s -> %s, want %s/%s -> %s",
			aws.ToString(a.Namespace), aws.ToString(a.ServiceAccount), aws.ToString(a.RoleArn),
			core.ClusterAutoscalerNamespace, core.ClusterAutoscalerServiceAccount, f.roles[role])
	}
}

// Auto Mode scales its own nodes and gets no cluster-autoscaler addon, so
// it must get no identity for one either.
func TestReconcile_AutoModeGetsNoClusterAutoscalerIdentity(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	spec.Autopilot = true
	spec.NodePools = nil
	f.activeCluster(spec)

	if _, err := NewClusterProvisioner(f.clients()).Reconcile(t.Context(), spec); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if f.called("CreatePodIdentityAssociation") {
		t.Error("an Auto Mode cluster was given a cluster-autoscaler identity")
	}
	if _, ok := f.roles[names{spec}.clusterAutoscalerRole()]; ok {
		t.Error("an Auto Mode cluster was given a cluster-autoscaler role")
	}
}

// IAM refuses to delete a role that still carries an inline policy, so
// teardown has to remove the autoscaler's before deleting its role.
func TestDelete_RemovesTheClusterAutoscalerRole(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	f.activeCluster(spec)
	p := NewClusterProvisioner(f.clients())
	if err := p.ensureClusterAutoscalerIdentity(t.Context(), spec, nil); err != nil {
		t.Fatalf("ensureClusterAutoscalerIdentity: %v", err)
	}

	if err := p.Delete(t.Context(), spec); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	role := names{spec}.clusterAutoscalerRole()
	if _, ok := f.roles[role]; ok {
		t.Errorf("role %s survived Delete", role)
	}
	if len(f.inlinePolicies[role]) != 0 {
		t.Errorf("inline policies on %s survived Delete: %v", role, f.inlinePolicies[role])
	}
}
