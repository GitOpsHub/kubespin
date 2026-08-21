package core

import (
	"errors"
	"strings"
	"testing"
)

func validSpec() ClusterSpec {
	return ClusterSpec{
		ID:                "team-payments-prod",
		Provider:          ProviderAWS,
		Region:            "us-east-1",
		Access:            AccessPrivate,
		KubernetesVersion: "1.34",
		NodePools: []NodePool{{
			Name:         "default",
			InstanceType: "m6i.large",
			MinSize:      2,
			MaxSize:      10,
			DesiredSize:  3,
		}},
		Size:    SizeSmall,
		Subnets: []string{"subnet-aaa", "subnet-bbb"},
	}
}

func TestClusterSpecValidate_Valid(t *testing.T) {
	if err := validSpec().Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}

	public := validSpec()
	public.Access = AccessPublic
	public.AuthorizedCIDRs = []string{"203.0.113.0/24"}
	if err := public.Validate(); err != nil {
		t.Fatalf("valid public spec rejected: %v", err)
	}
}

func TestClusterSpecValidate_Invalid(t *testing.T) {
	tests := map[string]struct {
		mutate  func(*ClusterSpec)
		wantMsg string
	}{
		"empty id":           {func(s *ClusterSpec) { s.ID = "" }, "cluster id is required"},
		"uppercase id":       {func(s *ClusterSpec) { s.ID = "Team-Prod" }, "cluster id"},
		"id with underscore": {func(s *ClusterSpec) { s.ID = "team_prod" }, "cluster id"},
		"id starting digit":  {func(s *ClusterSpec) { s.ID = "1team" }, "cluster id"},
		"id too short":       {func(s *ClusterSpec) { s.ID = "ab" }, "cluster id"},
		"unknown provider":   {func(s *ClusterSpec) { s.Provider = "oracle" }, "provider"},
		"empty region":       {func(s *ClusterSpec) { s.Region = "" }, "region is required"},
		"unknown access":     {func(s *ClusterSpec) { s.Access = "semi-private" }, "access"},
		"bad k8s version":    {func(s *ClusterSpec) { s.KubernetesVersion = "v1.34.2" }, "kubernetesVersion"},
		"no node pools":      {func(s *ClusterSpec) { s.NodePools = nil }, "at least one node pool"},
		"invalid size":       {func(s *ClusterSpec) { s.Size = "extra-large" }, "size"},
		"private with cidrs": {
			func(s *ClusterSpec) { s.AuthorizedCIDRs = []string{"10.0.0.0/8"} },
			"meaningless for a private cluster",
		},
		"duplicate node pool": {
			func(s *ClusterSpec) { s.NodePools = append(s.NodePools, s.NodePools[0]) },
			"duplicate node pool name",
		},
		"invalid override": {
			func(s *ClusterSpec) { s.Overrides = []AddonOverride{{Name: "Cert Manager"}} },
			"not a valid name",
		},
		"duplicate override": {
			func(s *ClusterSpec) {
				s.Overrides = []AddonOverride{{Name: "cert-manager"}, {Name: "cert-manager"}}
			},
			"duplicate override",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			tc.mutate(&s)

			err := s.Validate()
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !errors.Is(err, ErrInvalidSpec) {
				t.Errorf("error %v does not wrap ErrInvalidSpec", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tc.wantMsg)
			}
		})
	}
}

// Every provider's EnsureNetwork creates a network when none is supplied, so
// an empty Subnets is valid on every cloud, not just Azure.
func TestClusterSpecValidate_AllowsEmptySubnets(t *testing.T) {
	for _, provider := range []Provider{ProviderAWS, ProviderGCP, ProviderAzure} {
		t.Run(string(provider), func(t *testing.T) {
			s := validSpec()
			s.Provider = provider
			s.Subnets = nil

			if err := s.Validate(); err != nil {
				t.Fatalf("%s spec with no subnets rejected: %v", provider, err)
			}
		})
	}
}

func TestClusterSpecValidate_CIDRFields(t *testing.T) {
	tests := map[string]struct {
		mutate  func(*ClusterSpec)
		wantErr bool
	}{
		"valid vpc cidr":       {func(s *ClusterSpec) { s.VPCCIDR = "10.0.0.0/16" }, false},
		"valid vnet cidr":      {func(s *ClusterSpec) { s.VNetCIDR = "10.0.0.0/16" }, false},
		"valid subnet cidr":    {func(s *ClusterSpec) { s.SubnetCIDR = "10.0.1.0/24" }, false},
		"invalid vpc cidr":     {func(s *ClusterSpec) { s.VPCCIDR = "not-a-cidr" }, true},
		"invalid vnet cidr":    {func(s *ClusterSpec) { s.VNetCIDR = "not-a-cidr" }, true},
		"invalid subnet cidr":  {func(s *ClusterSpec) { s.SubnetCIDR = "10.0.1.0" }, true},
		"empty cidrs are fine": {func(s *ClusterSpec) {}, false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			tc.mutate(&s)

			err := s.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestClusterSpecValidate_ReportsEveryProblemAtOnce(t *testing.T) {
	// Fixing a spec one error per run is miserable, so Validate joins failures.
	s := validSpec()
	s.ID = ""
	s.Region = ""
	s.Provider = "oracle"

	err := s.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{"cluster id", "region", "provider"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error %q is missing %q", err, want)
		}
	}
}

func TestNodePoolValidate(t *testing.T) {
	tests := map[string]struct {
		pool    NodePool
		wantErr bool
	}{
		"valid":            {NodePool{Name: "np", InstanceType: "m6i.large", MinSize: 1, MaxSize: 3, DesiredSize: 2}, false},
		"min equals max":   {NodePool{Name: "np", InstanceType: "m6i.large", MinSize: 2, MaxSize: 2, DesiredSize: 2}, false},
		"scale to zero":    {NodePool{Name: "np", InstanceType: "m6i.large", MinSize: 0, MaxSize: 5, DesiredSize: 0}, false},
		"no name":          {NodePool{InstanceType: "m6i.large", MaxSize: 1, DesiredSize: 1}, true},
		"no instance type": {NodePool{Name: "np", MaxSize: 1, DesiredSize: 1}, true},
		"min above max":    {NodePool{Name: "np", InstanceType: "m6i.large", MinSize: 5, MaxSize: 2, DesiredSize: 2}, true},
		"desired above max": {
			NodePool{Name: "np", InstanceType: "m6i.large", MinSize: 1, MaxSize: 3, DesiredSize: 9}, true,
		},
		"desired below min": {
			NodePool{Name: "np", InstanceType: "m6i.large", MinSize: 3, MaxSize: 9, DesiredSize: 1}, true,
		},
		"zero max": {NodePool{Name: "np", InstanceType: "m6i.large"}, true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := tc.pool.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidSpec) {
				t.Errorf("error %v does not wrap ErrInvalidSpec", err)
			}
		})
	}
}

func TestProviderAndAccessValid(t *testing.T) {
	for _, p := range Providers() {
		if !p.Valid() {
			t.Errorf("provider %s reported invalid", p)
		}
	}
	if Provider("oracle").Valid() {
		t.Error("unknown provider reported valid")
	}
	if !AccessPrivate.Valid() || !AccessPublic.Valid() {
		t.Error("known access mode reported invalid")
	}
	if Access("semi-private").Valid() {
		t.Error("unknown access mode reported valid")
	}
}
