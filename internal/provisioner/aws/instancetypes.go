package aws

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// Shape of an instance type core.InstanceTypeAuto may pick. 2 vCPU / 4 GiB is
// the smallest node that reliably schedules the default (--size small) addon
// set — the same floor the old fixed t3.medium default was chosen for — and
// holding vCPUs at exactly 2 keeps every candidate at the cheapest node size
// rather than letting a larger type in on a lower per-vCPU price.
const (
	autoVCPUs        = 2
	autoMinMemoryMiB = 4096

	// autoInstanceTypeCount is how many of the cheapest types a node group
	// gets. More than one lets EKS's price-capacity-optimized spot allocation
	// fall back to the next-cheapest pool when one runs dry instead of
	// failing to launch; keeping it small keeps every pool it can land in
	// near the bottom of the price list.
	autoInstanceTypeCount = 6

	spotProductDescription = "Linux/UNIX"
)

// resolveAutoInstanceTypes returns the cheapest instance types, by current
// spot price, that fit the auto shape and are offered in every availability
// zone the cluster's subnets span, cheapest first.
//
// Prices are read live from DescribeSpotPriceHistory at node-group creation,
// so the choice tracks the market rather than a list baked into the binary.
// A type's price is its highest across those zones: the node group can
// launch into any of them, so the worst zone is the price actually risked.
func (p *ClusterProvisioner) resolveAutoInstanceTypes(ctx context.Context, spec core.ClusterSpec) ([]string, error) {
	zones, err := p.subnetZones(ctx, spec.Subnets)
	if err != nil {
		return nil, err
	}

	candidates, err := p.autoCandidateTypes(ctx)
	if err != nil {
		return nil, err
	}
	if len(zones) > 0 {
		if candidates, err = p.offeredInAllZones(ctx, candidates, zones); err != nil {
			return nil, err
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no spot instance type with %d vCPUs and at least %d MiB memory is offered in %s",
			autoVCPUs, autoMinMemoryMiB, strings.Join(zones, ", "))
	}

	prices, err := p.spotPrices(ctx, candidates, zones)
	if err != nil {
		return nil, err
	}

	priced := make([]string, 0, len(prices))
	for t := range prices {
		priced = append(priced, t)
	}
	if len(priced) == 0 {
		return nil, fmt.Errorf("no current spot price for any of %d candidate instance types", len(candidates))
	}
	slices.SortFunc(priced, func(a, b string) int {
		if prices[a] != prices[b] {
			if prices[a] < prices[b] {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	if len(priced) > autoInstanceTypeCount {
		priced = priced[:autoInstanceTypeCount]
	}

	p.c.logger.Info("Selected Spot Instance Types", "types", priced,
		"cheapest", priced[0], "pricePerHour", prices[priced[0]])
	return priced, nil
}

// subnetZones returns the distinct availability zones of the given subnets,
// or none when there are no subnets to look up.
func (p *ClusterProvisioner) subnetZones(ctx context.Context, subnets []string) ([]string, error) {
	if len(subnets) == 0 {
		return nil, nil
	}
	out, err := p.c.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: subnets})
	if err != nil {
		return nil, fmt.Errorf("describing subnets %v: %w", subnets, err)
	}
	var zones []string
	for _, s := range out.Subnets {
		if az := aws.ToString(s.AvailabilityZone); az != "" && !slices.Contains(zones, az) {
			zones = append(zones, az)
		}
	}
	slices.Sort(zones)
	return zones, nil
}

// autoCandidateTypes lists every current-generation, spot-capable x86_64
// Nitro instance type matching the auto shape. x86_64 only: EKS picks the node
// AMI from the instance types, and a node group cannot mix architectures.
func (p *ClusterProvisioner) autoCandidateTypes(ctx context.Context) ([]string, error) {
	input := &ec2.DescribeInstanceTypesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("current-generation"), Values: []string{"true"}},
			{Name: aws.String("bare-metal"), Values: []string{"false"}},
			// The vpc-cni add-on runs with prefix delegation, which only Nitro
			// instances support.
			{Name: aws.String("hypervisor"), Values: []string{"nitro"}},
			{Name: aws.String("supported-usage-class"), Values: []string{"spot"}},
			{Name: aws.String("processor-info.supported-architecture"), Values: []string{"x86_64"}},
			{Name: aws.String("vcpu-info.default-vcpus"), Values: []string{strconv.Itoa(autoVCPUs)}},
		},
	}

	var types []string
	for {
		out, err := p.c.ec2.DescribeInstanceTypes(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("describing instance types: %w", err)
		}
		for _, it := range out.InstanceTypes {
			if it.GpuInfo != nil || it.MemoryInfo == nil || aws.ToInt64(it.MemoryInfo.SizeInMiB) < autoMinMemoryMiB {
				continue
			}
			types = append(types, string(it.InstanceType))
		}
		if aws.ToString(out.NextToken) == "" {
			break
		}
		input.NextToken = out.NextToken
	}
	slices.Sort(types)
	return types, nil
}

// offeredInAllZones keeps only the candidates offered in every zone, so the
// node group never lists a type some of its subnets cannot launch.
func (p *ClusterProvisioner) offeredInAllZones(ctx context.Context, candidates, zones []string) ([]string, error) {
	input := &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: ec2types.LocationTypeAvailabilityZone,
		Filters: []ec2types.Filter{
			{Name: aws.String("location"), Values: zones},
			{Name: aws.String("instance-type"), Values: candidates},
		},
	}

	offered := map[string]int{}
	for {
		out, err := p.c.ec2.DescribeInstanceTypeOfferings(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("describing instance type offerings in %v: %w", zones, err)
		}
		for _, o := range out.InstanceTypeOfferings {
			offered[string(o.InstanceType)]++
		}
		if aws.ToString(out.NextToken) == "" {
			break
		}
		input.NextToken = out.NextToken
	}

	var kept []string
	for _, t := range candidates {
		if offered[t] == len(zones) {
			kept = append(kept, t)
		}
	}
	return kept, nil
}

// spotPrices returns each candidate's current Linux spot price in USD/hour,
// taking the highest across zones. A type with no price in some zone is
// dropped, since its spot pool there cannot be relied on.
func (p *ClusterProvisioner) spotPrices(ctx context.Context, candidates, zones []string) (map[string]float64, error) {
	instanceTypes := make([]ec2types.InstanceType, len(candidates))
	for i, t := range candidates {
		instanceTypes[i] = ec2types.InstanceType(t)
	}
	input := &ec2.DescribeSpotPriceHistoryInput{
		// A StartTime of now returns just the price in effect right now for
		// each type and zone, rather than the day of history the API
		// otherwise defaults to.
		StartTime:           aws.Time(time.Now()),
		ProductDescriptions: []string{spotProductDescription},
		InstanceTypes:       instanceTypes,
	}

	type key struct{ instanceType, zone string }
	latest := map[key]ec2types.SpotPrice{}
	for {
		out, err := p.c.ec2.DescribeSpotPriceHistory(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("describing spot prices: %w", err)
		}
		for _, sp := range out.SpotPriceHistory {
			k := key{string(sp.InstanceType), aws.ToString(sp.AvailabilityZone)}
			if prev, ok := latest[k]; !ok || aws.ToTime(sp.Timestamp).After(aws.ToTime(prev.Timestamp)) {
				latest[k] = sp
			}
		}
		if aws.ToString(out.NextToken) == "" {
			break
		}
		input.NextToken = out.NextToken
	}

	perType := map[string]map[string]float64{}
	for k, sp := range latest {
		if len(zones) > 0 && !slices.Contains(zones, k.zone) {
			continue
		}
		price, err := strconv.ParseFloat(aws.ToString(sp.SpotPrice), 64)
		if err != nil {
			continue
		}
		if perType[k.instanceType] == nil {
			perType[k.instanceType] = map[string]float64{}
		}
		perType[k.instanceType][k.zone] = price
	}

	prices := map[string]float64{}
	for t, byZone := range perType {
		if len(zones) > 0 && len(byZone) < len(zones) {
			continue
		}
		var highest float64
		for _, price := range byZone {
			highest = max(highest, price)
		}
		prices[t] = highest
	}
	return prices, nil
}
