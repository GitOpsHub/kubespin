package core

import (
	"errors"
	"strings"
	"testing"
)

func validAddon() AddonRef {
	return AddonRef{
		Name:       "cert-manager",
		Chart:      "cert-manager",
		Repository: "https://charts.jetstack.io",
		Version:    "v1.16.2",
		Namespace:  "cert-manager",
	}
}

func TestAddonRefValidate(t *testing.T) {
	if err := validAddon().Validate(); err != nil {
		t.Fatalf("valid addon rejected: %v", err)
	}

	tests := map[string]struct {
		mutate  func(*AddonRef)
		wantMsg string
	}{
		"no name":       {func(a *AddonRef) { a.Name = "" }, "not a valid name"},
		"bad name":      {func(a *AddonRef) { a.Name = "Cert_Manager" }, "not a valid name"},
		"no chart":      {func(a *AddonRef) { a.Chart = "" }, "chart is required"},
		"no repository": {func(a *AddonRef) { a.Repository = "" }, "repository is required"},
		"no version":    {func(a *AddonRef) { a.Version = "" }, "version is required"},
		"no namespace":  {func(a *AddonRef) { a.Namespace = "" }, "namespace is required"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			a := validAddon()
			tc.mutate(&a)

			err := a.Validate()
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

func TestProfileValidate(t *testing.T) {
	p := Profile{Name: "small", Addons: []AddonRef{validAddon()}}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}

	t.Run("rejects duplicate addons", func(t *testing.T) {
		// Two Argo CD Applications cannot share a name, so this has to fail at
		// resolution time rather than at sync time on the cluster.
		dup := Profile{Name: "small", Addons: []AddonRef{validAddon(), validAddon()}}
		err := dup.Validate()
		if err == nil || !strings.Contains(err.Error(), "duplicate addon name") {
			t.Fatalf("duplicate addons not rejected: %v", err)
		}
	})

	t.Run("rejects empty addon set", func(t *testing.T) {
		empty := Profile{Name: "small"}
		if err := empty.Validate(); err == nil {
			t.Fatal("expected an error for a profile with no addons")
		}
	})

	t.Run("rejects empty name", func(t *testing.T) {
		unnamed := Profile{Addons: []AddonRef{validAddon()}}
		if err := unnamed.Validate(); err == nil {
			t.Fatal("expected an error for a profile with no name")
		}
	})
}

func TestAddonRefSupportsProvider(t *testing.T) {
	agnostic := validAddon()
	if !agnostic.SupportsProvider(ProviderGCP) {
		t.Error("addon with no Providers should support every provider")
	}

	gated := validAddon()
	gated.Providers = []Provider{ProviderAWS}
	if !gated.SupportsProvider(ProviderAWS) {
		t.Error("addon gated to aws should support aws")
	}
	if gated.SupportsProvider(ProviderGCP) {
		t.Error("addon gated to aws should not support gcp")
	}
}

func TestProfileForProvider(t *testing.T) {
	agnostic := validAddon()
	awsOnly := AddonRef{
		Name: "aws-only", Chart: "aws-only", Repository: "oci://example",
		Version: "1.0.0", Namespace: "aws-only", Providers: []Provider{ProviderAWS},
	}
	p := Profile{Name: "medium", Addons: []AddonRef{agnostic, awsOnly}}

	forAWS := p.ForProvider(ProviderAWS)
	if len(forAWS.Addons) != 2 {
		t.Fatalf("ForProvider(aws) dropped an addon it should keep: got %d addons", len(forAWS.Addons))
	}

	forGCP := p.ForProvider(ProviderGCP)
	if len(forGCP.Addons) != 1 || forGCP.Addons[0].Name != agnostic.Name {
		t.Fatalf("ForProvider(gcp) should drop the aws-only addon, got %+v", forGCP.Addons)
	}

	// The original profile's backing array must not be mutated by filtering.
	if len(p.Addons) != 2 {
		t.Fatalf("ForProvider mutated the source profile: %+v", p.Addons)
	}
}

func TestProfileForAutopilot(t *testing.T) {
	agnostic := validAddon()
	autoscaler := AddonRef{
		Name: "cluster-autoscaler", Chart: "cluster-autoscaler", Repository: "https://example",
		Version: "1.0.0", Namespace: "kube-system",
	}
	p := Profile{Name: "small", Addons: []AddonRef{agnostic, autoscaler}}

	forAutopilot := p.ForAutopilot(true)
	if len(forAutopilot.Addons) != 0 {
		t.Fatalf("ForAutopilot(true) should drop every addon, got %+v", forAutopilot.Addons)
	}

	forStandard := p.ForAutopilot(false)
	if len(forStandard.Addons) != 2 {
		t.Fatalf("ForAutopilot(false) should keep every addon, got %d", len(forStandard.Addons))
	}

	if len(p.Addons) != 2 {
		t.Fatalf("ForAutopilot mutated the source profile: %+v", p.Addons)
	}
}

func TestAddonOverrideValidate(t *testing.T) {
	if err := (AddonOverride{Name: "cert-manager"}).Validate(); err != nil {
		t.Fatalf("valid override rejected: %v", err)
	}
	if err := (AddonOverride{Name: "Cert Manager"}).Validate(); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("error = %v, want one wrapping ErrInvalidSpec", err)
	}
}

func TestClusterSizeValid(t *testing.T) {
	tests := map[string]struct {
		size  ClusterSize
		valid bool
	}{
		"small":   {SizeSmall, true},
		"medium":  {SizeMedium, true},
		"large":   {SizeLarge, true},
		"empty":   {"", false},
		"unknown": {"extra-large", false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tc.size.Valid(); got != tc.valid {
				t.Fatalf("Valid() = %v, want %v", got, tc.valid)
			}
		})
	}
}

func TestProfileValidate_DuplicateNamesNeedDisjointProviders(t *testing.T) {
	entry := func(providers ...Provider) AddonRef {
		return AddonRef{
			Name: "cluster-autoscaler", Chart: "cluster-autoscaler", Repository: "https://example",
			Version: "1.0.0", Namespace: "kube-system", Providers: providers,
		}
	}
	for _, tc := range []struct {
		name    string
		addons  []AddonRef
		wantErr bool
	}{
		{"disjoint", []AddonRef{entry(ProviderAWS), entry(ProviderGCP, ProviderAzure)}, false},
		{"overlapping", []AddonRef{entry(ProviderAWS), entry(ProviderAWS, ProviderGCP)}, true},
		{"one ungated", []AddonRef{entry(), entry(ProviderAWS)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Profile{Name: "small", Addons: tc.addons}.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
