package repo

import (
	"context"
	"testing"
)

func TestMemory_Archive(t *testing.T) {
	m := NewMemory()
	spec := testSpec()

	if err := m.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if m.Archived(spec) {
		t.Fatal("expected a freshly created repo not to be archived")
	}

	if err := m.Archive(context.Background(), spec); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if !m.Archived(spec) {
		t.Error("expected the repo to be archived")
	}

	// Idempotent and converges on an absent repo.
	if err := m.Archive(context.Background(), spec); err != nil {
		t.Fatalf("second Archive: %v", err)
	}
	if err := NewMemory().Archive(context.Background(), testSpec()); err != nil {
		t.Fatalf("Archive on an absent repo should converge, not fail: %v", err)
	}
}
