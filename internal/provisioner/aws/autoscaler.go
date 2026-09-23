package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
)

// clusterAutoscalerPolicyName is the inline policy on the autoscaler's
// role. AWS publishes no managed policy for cluster-autoscaler.
const clusterAutoscalerPolicyName = "cluster-autoscaler"

// ensureClusterAutoscalerIdentity gives the catalog's cluster-autoscaler
// addon the AWS permissions it needs to resize the cluster's managed node
// groups: a role scoped to the node groups' Auto Scaling groups, bound to the
// addon's service account through EKS Pod Identity.
//
// AWS ships no EKS add-on for cluster-autoscaler, so it stays a catalog Helm
// chart Argo CD renders, and its Pod Identity association is created here
// directly rather than through CreateAddon. The chart's values only name the
// service account; no role ARN (or AWS account ID) has to reach the catalog.
// It relies on the eks-pod-identity-agent add-on workloadAddons installs.
func (p *ClusterProvisioner) ensureClusterAutoscalerIdentity(
	ctx context.Context, spec core.ClusterSpec, change *provisioner.Change,
) error {
	role := names{spec}.clusterAutoscalerRole()
	roleARN, err := p.ensureRole(ctx, role, podIdentityTrust(), nil)
	if err != nil {
		return fmt.Errorf("ensuring cluster-autoscaler role: %w", err)
	}
	if err := p.ensureInlinePolicy(ctx, role, clusterAutoscalerPolicyName, clusterAutoscalerPolicy(spec)); err != nil {
		return err
	}

	listed, err := p.c.eks.ListPodIdentityAssociations(ctx, &eks.ListPodIdentityAssociationsInput{
		ClusterName:    aws.String(names{spec}.cluster()),
		Namespace:      aws.String(core.ClusterAutoscalerNamespace),
		ServiceAccount: aws.String(core.ClusterAutoscalerServiceAccount),
	})
	if err != nil {
		return fmt.Errorf("listing pod identity associations for %s: %w", spec.ID, err)
	}
	// The role name is deterministic, so an existing association for this
	// service account already points at it.
	if len(listed.Associations) > 0 {
		return nil
	}

	if _, err := p.c.eks.CreatePodIdentityAssociation(ctx, &eks.CreatePodIdentityAssociationInput{
		ClusterName:    aws.String(names{spec}.cluster()),
		Namespace:      aws.String(core.ClusterAutoscalerNamespace),
		ServiceAccount: aws.String(core.ClusterAutoscalerServiceAccount),
		RoleArn:        aws.String(roleARN),
		Tags:           tags(spec),
	}); err != nil {
		var exists *ekstypes.ResourceInUseException
		if errors.As(err, &exists) {
			return nil
		}
		return fmt.Errorf("creating pod identity association for cluster-autoscaler on %s: %w", spec.ID, err)
	}
	p.c.logger.Info("Bound Cluster Autoscaler Identity", "role", role)
	record(change, "bind cluster-autoscaler identity")
	return nil
}

// ensureInlinePolicy puts policy on role unless the role already carries an
// identical one, so an unchanged apply makes no IAM writes.
func (p *ClusterProvisioner) ensureInlinePolicy(ctx context.Context, role, name string, policy map[string]any) error {
	doc, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("rendering policy %s for %s: %w", name, role, err)
	}

	current, err := p.c.iam.GetRolePolicy(ctx, &iam.GetRolePolicyInput{
		RoleName:   aws.String(role),
		PolicyName: aws.String(name),
	})
	if err == nil && samePolicy(aws.ToString(current.PolicyDocument), doc) {
		return nil
	}
	var missing *iamtypes.NoSuchEntityException
	if err != nil && !errors.As(err, &missing) {
		return fmt.Errorf("getting policy %s on %s: %w", name, role, err)
	}

	if _, err := p.c.iam.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       aws.String(role),
		PolicyName:     aws.String(name),
		PolicyDocument: aws.String(string(doc)),
	}); err != nil {
		return fmt.Errorf("putting policy %s on %s: %w", name, role, err)
	}
	p.c.logger.Info("Updated IAM Role Policy", "role", role, "policy", name)
	return nil
}

// samePolicy compares an inline policy as IAM returns it (URL-encoded JSON)
// against a rendered one, semantically rather than byte for byte.
func samePolicy(encoded string, want []byte) bool {
	decoded, err := url.QueryUnescape(encoded)
	if err != nil {
		return false
	}
	return sameJSON(decoded, string(want))
}

// sameJSON reports whether two JSON documents are semantically equal,
// ignoring key order and whitespace.
func sameJSON(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// podIdentityTrust lets EKS Pod Identity assume the role on behalf of
// whichever service account an association binds it to.
func podIdentityTrust() map[string]any {
	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect":    "Allow",
			"Action":    []string{"sts:AssumeRole", "sts:TagSession"},
			"Principal": map[string]any{"Service": "pods.eks.amazonaws.com"},
		}},
	}
}

// clusterAutoscalerPolicy is cluster-autoscaler's documented AWS policy:
// read-only discovery everywhere, but resizing and terminating only Auto
// Scaling groups tagged as owned by this cluster. EKS tags every managed
// node group's Auto Scaling group that way, which is also what the
// autoscaler's auto-discovery keys off.
func clusterAutoscalerPolicy(spec core.ClusterSpec) map[string]any {
	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{
			map[string]any{
				"Effect": "Allow",
				"Action": []string{
					"autoscaling:DescribeAutoScalingGroups",
					"autoscaling:DescribeAutoScalingInstances",
					"autoscaling:DescribeLaunchConfigurations",
					"autoscaling:DescribeScalingActivities",
					"autoscaling:DescribeTags",
					"ec2:DescribeImages",
					"ec2:DescribeInstanceTypes",
					"ec2:DescribeLaunchTemplateVersions",
					"ec2:GetInstanceTypesFromInstanceRequirements",
					"eks:DescribeNodegroup",
				},
				"Resource": "*",
			},
			map[string]any{
				"Effect": "Allow",
				"Action": []string{
					"autoscaling:SetDesiredCapacity",
					"autoscaling:TerminateInstanceInAutoScalingGroup",
				},
				"Resource": "*",
				"Condition": map[string]any{
					"StringEquals": map[string]any{
						"aws:ResourceTag/k8s.io/cluster-autoscaler/" + names{spec}.cluster(): "owned",
					},
				},
			},
		},
	}
}
