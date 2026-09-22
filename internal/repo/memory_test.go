package repo

import (
	"context"
	"testing"
)

func TestMemory_Delete(t *testing.T) {
	m := NewMemory()
	spec := testSpec()

	if err := m.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if m.Deleted(spec) {
		t.Fatal("expected a freshly created repo not to be deleted")
	}

	if err := m.Delete(context.Background(), spec); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !m.Deleted(spec) {
		t.Error("expected the repo to be deleted")
	}
	exists, err := m.Exists(context.Background(), spec)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Error("a deleted repo must not report as existing")
	}

	// Idempotent and converges on an absent repo.
	if err := m.Delete(context.Background(), spec); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if err := NewMemory().Delete(context.Background(), testSpec()); err != nil {
		t.Fatalf("Delete on an absent repo should converge, not fail: %v", err)
	}
}
