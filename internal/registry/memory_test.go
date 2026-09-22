package registry

import (
	"testing"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
)

func TestMemoryRegistry(t *testing.T) {
	runContract(t, func(_ *testing.T, clock *fakeClock) Registry {
		return NewMemory(WithClock(clock.Now))
	})
}

// Callers must not be able to mutate stored state through a returned record.
func TestMemoryDoesNotAliasStoredState(t *testing.T) {
	clock := newFakeClock()
	r := NewMemory(WithClock(clock.Now))
	rec := seed(t, r, clock, "team-alpha")

	if _, err := r.AcquireLease(t.Context(), rec.ClusterID, "runner-a", time.Minute); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	got, err := r.Get(t.Context(), rec.ClusterID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Lease.Holder = "impostor"

	again, err := r.Get(t.Context(), rec.ClusterID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if again.Lease.Holder != "runner-a" {
		t.Errorf("stored holder = %q, want runner-a: the caller mutated stored state", again.Lease.Holder)
	}
}

func TestLeaseExpired(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)

	if (Lease{ExpiresAt: now.Add(time.Minute)}).Expired(now) {
		t.Error("a future expiry reported as expired")
	}
	if !(Lease{ExpiresAt: now.Add(-time.Minute)}).Expired(now) {
		t.Error("a past expiry reported as live")
	}
	// Exactly at the deadline the lease is gone: holding past ExpiresAt would
	// overlap with whoever takes it next.
	if !(Lease{ExpiresAt: now}).Expired(now) {
		t.Error("a lease exactly at its expiry reported as live")
	}
}

func TestNewRecord(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	spec := core.ClusterSpec{
		ID:       "team-payments-prod",
		Provider: core.ProviderAWS,
		Region:   "us-east-1",
		Access:   core.AccessPrivate,
		Size:     core.SizeSmall,
	}

	rec := NewRecord(spec, now)

	if rec.Phase != core.PhasePending {
		t.Errorf("Phase = %s, want pending", rec.Phase)
	}
	if rec.Version != 1 {
		t.Errorf("Version = %d, want 1", rec.Version)
	}
	if !rec.CreatedAt.Equal(now) || !rec.UpdatedAt.Equal(now) {
		t.Errorf("timestamps = %v/%v, want both %v", rec.CreatedAt, rec.UpdatedAt, now)
	}
	if err := rec.Validate(); err != nil {
		t.Errorf("a record built from a valid spec does not validate: %v", err)
	}
}
