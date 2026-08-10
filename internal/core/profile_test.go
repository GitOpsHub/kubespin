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
	p := Profile{Name: "tier-small", Version: "1.0.0", Addons: []AddonRef{validAddon()}}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
	if p.Ref().String() != "tier-small@1.0.0" {
		t.Errorf("Ref().String() = %q", p.Ref().String())
	}

	t.Run("rejects duplicate addons", func(t *testing.T) {
		// Two Argo CD Applications cannot share a name, so this has to fail at
		// resolution time rather than at sync time on the cluster.
		dup := Profile{Name: "tier-small", Version: "1.0.0", Addons: []AddonRef{validAddon(), validAddon()}}
		err := dup.Validate()
		if err == nil || !strings.Contains(err.Error(), "duplicate addon name") {
			t.Fatalf("duplicate addons not rejected: %v", err)
		}
	})

	t.Run("rejects empty addon set", func(t *testing.T) {
		empty := Profile{Name: "tier-small", Version: "1.0.0"}
		if err := empty.Validate(); err == nil {
			t.Fatal("expected an error for a profile with no addons")
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
		Name: "karpenter", Chart: "karpenter", Repository: "oci://example",
		Version: "1.0.0", Namespace: "karpenter", Providers: []Provider{ProviderAWS},
	}
	p := Profile{Name: "tier-standard", Version: "1.0.0", Addons: []AddonRef{agnostic, awsOnly}}

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

func TestAddonOverrideValidate(t *testing.T) {
	if err := (AddonOverride{Name: "cert-manager"}).Validate(); err != nil {
		t.Fatalf("valid override rejected: %v", err)
	}
	if err := (AddonOverride{Name: "Cert Manager"}).Validate(); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("error = %v, want one wrapping ErrInvalidSpec", err)
	}
}

func TestProfileRefValidate(t *testing.T) {
	tests := map[string]struct {
		ref     ProfileRef
		wantErr bool
	}{
		"valid":       {ProfileRef{Name: "tier-regulated", Version: "2.1.0"}, false},
		"no name":     {ProfileRef{Version: "1.0.0"}, true},
		"bad name":    {ProfileRef{Name: "Tier Small", Version: "1.0.0"}, true},
		"no version":  {ProfileRef{Name: "tier-small"}, true},
		"all missing": {ProfileRef{}, true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if err := tc.ref.Validate(); (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
