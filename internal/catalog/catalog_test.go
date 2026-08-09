package catalog

import (
	"context"
	"errors"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/core"
)

func TestBuiltinResolver_Resolve(t *testing.T) {
	r := NewBuiltinResolver()

	profile, err := r.Resolve(context.Background(), core.ProfileRef{Name: "tier-small", Version: "1.0.0"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := profile.Validate(); err != nil {
		t.Errorf("resolved profile is invalid: %v", err)
	}
	if len(profile.Addons) == 0 {
		t.Error("expected at least one addon")
	}
}

func TestBuiltinResolver_Resolve_NotFound(t *testing.T) {
	r := NewBuiltinResolver()

	_, err := r.Resolve(context.Background(), core.ProfileRef{Name: "tier-nonexistent", Version: "1.0.0"})
	if !errors.Is(err, ErrProfileNotFound) {
		t.Errorf("error = %v, want one wrapping ErrProfileNotFound", err)
	}

	// A known name at an unknown version is not found either: versions are
	// pinned per-entry, not resolved by name alone.
	_, err = r.Resolve(context.Background(), core.ProfileRef{Name: "tier-small", Version: "9.9.9"})
	if !errors.Is(err, ErrProfileNotFound) {
		t.Errorf("error = %v, want one wrapping ErrProfileNotFound", err)
	}
}
