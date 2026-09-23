package aws

import (
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

// Prefix delegation has to be on before the node group exists: EKS fixes a
// managed node group's max-pods when it creates the group.
func TestCreate_EnablesPrefixDelegationBeforeCreatingNodeGroups(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	f.activeCluster(spec)

	if err := NewClusterProvisioner(f.clients()).Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	addon, ok := f.addons[addonVPCCNI]
	if !ok {
		t.Fatalf("addon %s was not installed", addonVPCCNI)
	}
	if !strings.Contains(aws.ToString(addon.ConfigurationValues), `"ENABLE_PREFIX_DELEGATION":"true"`) {
		t.Errorf("ConfigurationValues = %s, want prefix delegation on", aws.ToString(addon.ConfigurationValues))
	}

	addonAt := slices.Index(f.calls, "CreateAddon")
	nodeGroupAt := slices.Index(f.calls, "CreateNodegroup")
	if addonAt < 0 || nodeGroupAt < 0 || addonAt > nodeGroupAt {
		t.Errorf("calls %v: want the VPC CNI addon created before the node group", f.calls)
	}
}

// A cluster whose VPC CNI is already an EKS addon, but without prefix
// delegation (e.g. created by an older kubespin), is converged in place.
func TestReconcile_TurnsOnPrefixDelegationForAnExistingAddon(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	f.activeCluster(spec)
	f.withNodePool(spec, spec.NodePools[0])
	f.addons[addonVPCCNI] = &ekstypes.Addon{AddonName: aws.String(addonVPCCNI)}

	change, err := NewClusterProvisioner(f.clients()).Reconcile(t.Context(), spec)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !strings.Contains(aws.ToString(f.addons[addonVPCCNI].ConfigurationValues), "ENABLE_PREFIX_DELEGATION") {
		t.Errorf("ConfigurationValues = %s, want prefix delegation on", aws.ToString(f.addons[addonVPCCNI].ConfigurationValues))
	}
	if !slices.Contains(change.Details, "update addon "+addonVPCCNI) {
		t.Errorf("Details = %v, want the configuration change reported", change.Details)
	}
}

// Every add-on that needs AWS permissions gets them through Pod Identity,
// set on the add-on itself, with a role only EKS Pod Identity can assume.
func TestCreate_BindsAddonIdentitiesThroughPodIdentity(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	f.activeCluster(spec)

	if err := NewClusterProvisioner(f.clients()).Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, want := range []struct{ addon, serviceAccount, role string }{
		{addonEBSCSIDriver, "ebs-csi-controller-sa", names{spec}.ebsCSIRole()},
		{addonEFSCSIDriver, "efs-csi-controller-sa", names{spec}.efsCSIRole()},
		{addonExternalDNS, "external-dns", names{spec}.externalDNSRole()},
	} {
		addon, ok := f.addons[want.addon]
		if !ok {
			t.Errorf("addon %s was not installed", want.addon)
			continue
		}
		if addon.ServiceAccountRoleArn != nil {
			t.Errorf("addon %s was bound through IRSA, want Pod Identity", want.addon)
		}
		if len(addon.PodIdentityAssociations) != 1 {
			t.Errorf("addon %s has %d pod identity associations, want 1", want.addon, len(addon.PodIdentityAssociations))
		}
		if !strings.Contains(f.rolePolicy[want.role], "pods.eks.amazonaws.com") {
			t.Errorf("role %s does not trust EKS Pod Identity: %s", want.role, f.rolePolicy[want.role])
		}
	}
	if len(f.oidc) != 0 {
		t.Errorf("an IAM OIDC provider was registered; Pod Identity needs none: %v", f.oidc)
	}

	// external-dns must only claim records it created, so clusters sharing a
	// hosted zone leave each other's alone.
	if cfg := aws.ToString(f.addons[addonExternalDNS].ConfigurationValues); !strings.Contains(cfg, `"txtOwnerId":"`+spec.ID.String()+`"`) {
		t.Errorf("external-dns configuration = %s, want txtOwnerId %s", cfg, spec.ID)
	}
	for _, name := range []string{addonCertManager, addonFluentBit, addonCoreDNS, addonKubeProxy, addonPodIdentityAgent} {
		if _, ok := f.addons[name]; !ok {
			t.Errorf("addon %s was not installed", name)
		}
	}
}

// Auto Mode ships its own networking, DNS, block storage and Pod Identity
// agent; only EFS is added.
func TestCreate_AutoModeInstallsOnlyWhatItLacks(t *testing.T) {
	f := newFakeAWS()
	spec := testSpec()
	spec.Autopilot = true
	spec.NodePools = nil
	f.activeCluster(spec)

	if err := NewClusterProvisioner(f.clients()).Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var installed []string
	for name := range f.addons {
		installed = append(installed, name)
	}
	if !slices.Equal(installed, []string{addonEFSCSIDriver}) {
		t.Errorf("installed %v on Auto Mode, want only %s", installed, addonEFSCSIDriver)
	}
}
