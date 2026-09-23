package aws

import (
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// autoSpec is testSpec with its pool switched to spot + core.InstanceTypeAuto.
func autoSpec() core.ClusterSpec {
	spec := testSpec()
	spec.NodePools[0].InstanceType = core.InstanceTypeAuto
	spec.NodePools[0].CapacityType = core.CapacityTypeSpot
	return spec
}

// withSpotMarket seeds the fake with subnet-aaa/subnet-bbb in two zones and a
// spot market whose cheapest entries are each disqualified for a different
// reason, so only a correct filter picks the right types.
func (f *fakeAWS) withSpotMarket() {
	f.subnets["subnet-aaa"] = &ec2types.Subnet{SubnetId: aws.String("subnet-aaa"), AvailabilityZone: aws.String("us-east-1a")}
	f.subnets["subnet-bbb"] = &ec2types.Subnet{SubnetId: aws.String("subnet-bbb"), AvailabilityZone: aws.String("us-east-1b")}

	typeInfo := func(name string, memMiB int64, gpu bool) ec2types.InstanceTypeInfo {
		info := ec2types.InstanceTypeInfo{
			InstanceType: ec2types.InstanceType(name),
			MemoryInfo:   &ec2types.MemoryInfo{SizeInMiB: aws.Int64(memMiB)},
		}
		if gpu {
			info.GpuInfo = &ec2types.GpuInfo{}
		}
		return info
	}
	f.instanceTypes = []ec2types.InstanceTypeInfo{
		typeInfo("t3.small", 2048, false),  // too little memory
		typeInfo("g4dn.large", 8192, true), // GPU
		typeInfo("c7a.large", 4096, false), // not offered in us-east-1b
		typeInfo("t3.medium", 4096, false),
		typeInfo("t3a.medium", 4096, false),
		typeInfo("c5.large", 4096, false),
		typeInfo("c6i.large", 4096, false),
		typeInfo("m5.large", 8192, false),
		typeInfo("m6i.large", 8192, false),
		typeInfo("r5.large", 16384, false),
	}

	now := time.Now()
	for _, tp := range []struct {
		name   string
		aPrice string
		bPrice string
	}{
		{"t3.small", "0.0050", "0.0050"},
		{"g4dn.large", "0.0010", "0.0010"},
		{"c7a.large", "0.0020", ""},
		// t3a.medium is cheaper in 1a but dearer than t3.medium in 1b; the
		// worst zone is what counts.
		{"t3.medium", "0.0125", "0.0125"},
		{"t3a.medium", "0.0100", "0.0130"},
		{"c5.large", "0.0300", "0.0310"},
		{"c6i.large", "0.0280", "0.0290"},
		{"m5.large", "0.0350", "0.0340"},
		{"m6i.large", "0.0360", "0.0360"},
		{"r5.large", "0.0400", "0.0400"},
	} {
		for az, price := range map[string]string{"us-east-1a": tp.aPrice, "us-east-1b": tp.bPrice} {
			if price == "" {
				continue
			}
			f.offerings = append(f.offerings, ec2types.InstanceTypeOffering{
				InstanceType: ec2types.InstanceType(tp.name), Location: aws.String(az),
			})
			f.spotPrices = append(f.spotPrices,
				// A stale, lower price must lose to the current one.
				ec2types.SpotPrice{
					InstanceType: ec2types.InstanceType(tp.name), AvailabilityZone: aws.String(az),
					SpotPrice: aws.String("0.0001"), Timestamp: aws.Time(now.Add(-time.Hour)),
				},
				ec2types.SpotPrice{
					InstanceType: ec2types.InstanceType(tp.name), AvailabilityZone: aws.String(az),
					SpotPrice: aws.String(price), Timestamp: aws.Time(now),
				},
			)
		}
	}
}

func TestCreate_AutoInstanceType_PicksTheCheapestSpotTypes(t *testing.T) {
	f := newFakeAWS()
	f.withSpotMarket()
	spec := autoSpec()
	f.activeCluster(spec)

	if err := NewClusterProvisioner(f.clients()).Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ng, ok := f.nodeGroups[names{spec}.nodeGroup(spec.NodePools[0].Name)]
	if !ok {
		t.Fatalf("node group %s was not created", spec.NodePools[0].Name)
	}
	want := []string{"t3.medium", "t3a.medium", "c6i.large", "c5.large", "m5.large", "m6i.large"}
	if !slices.Equal(ng.InstanceTypes, want) {
		t.Errorf("InstanceTypes = %v, want %v", ng.InstanceTypes, want)
	}
	if ng.CapacityType != ekstypes.CapacityTypesSpot {
		t.Errorf("CapacityType = %s, want %s", ng.CapacityType, ekstypes.CapacityTypesSpot)
	}
}

func TestCreate_AutoInstanceType_FailsWhenNothingQualifies(t *testing.T) {
	f := newFakeAWS()
	f.withSpotMarket()
	f.instanceTypes = f.instanceTypes[:3] // only the disqualified types
	spec := autoSpec()
	f.activeCluster(spec)

	if err := NewClusterProvisioner(f.clients()).Create(t.Context(), spec); err == nil {
		t.Fatal("Create succeeded with no qualifying instance type, want an error")
	}
	if f.called("CreateNodegroup") {
		t.Error("CreateNodegroup was called without any instance type to give it")
	}
}

// The choice is made once, at creation: an existing auto node group must not
// cost pricing calls (or look like drift) on every apply.
func TestReconcile_AutoInstanceType_DoesNotReresolveAnExistingNodeGroup(t *testing.T) {
	f := newFakeAWS()
	f.withSpotMarket()
	spec := autoSpec()
	f.activeCluster(spec)
	f.withNodePool(spec, spec.NodePools[0])

	if _, err := NewClusterProvisioner(f.clients()).Reconcile(t.Context(), spec); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if f.called("DescribeSpotPriceHistory", "CreateNodegroup", "UpdateNodegroupConfig") {
		t.Errorf("unexpected calls reconciling an unchanged auto node group: %v", f.calls)
	}
}
