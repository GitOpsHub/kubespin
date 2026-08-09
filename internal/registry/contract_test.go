package registry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// This file is the registry contract: every behaviour callers are entitled to
// rely on, expressed once and run against every implementation. The in-memory
// registry is not allowed weaker semantics than DynamoDB — if it were, the
// orchestrator tests built on it would pass while production failed.
//
// runContract is invoked by memory_test.go and, behind the integration build
// tag, by dynamo_integration_test.go.

// fakeClock is a controllable time source, so lease expiry is testable without
// sleeping through a TTL.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// factory builds a fresh, empty registry sharing the given clock.
type factory func(t *testing.T, clock *fakeClock) Registry

func testRecord(id core.ClusterID, provider core.Provider, clock *fakeClock) Record {
	return Record{
		ClusterID: id,
		Phase:     core.PhasePending,
		Provider:  provider,
		Region:    "us-east-1",
		Access:    core.AccessPrivate,
		Profile:   core.ProfileRef{Name: "tier-small", Version: "1.0.0"},
		Version:   1,
		CreatedAt: clock.Now(),
		UpdatedAt: clock.Now(),
	}
}

// seed creates a record and fails the test if it cannot.
func seed(t *testing.T, r Registry, clock *fakeClock, id core.ClusterID) Record {
	t.Helper()

	rec, err := r.Create(context.Background(), testRecord(id, core.ProviderAWS, clock))
	if err != nil {
		t.Fatalf("seeding %s: %v", id, err)
	}
	return rec
}

func runContract(t *testing.T, newRegistry factory) {
	t.Helper()

	t.Run("get missing cluster", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)

		if _, err := r.Get(context.Background(), "absent-cluster"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want one wrapping ErrNotFound", err)
		}
	})

	t.Run("create then get round-trips every field", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		want := testRecord("team-payments-prod", core.ProviderGCP, clock)
		want.Access = core.AccessPublic

		if _, err := r.Create(context.Background(), want); err != nil {
			t.Fatalf("Create: %v", err)
		}

		got, err := r.Get(context.Background(), want.ClusterID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}

		if got.ClusterID != want.ClusterID || got.Phase != want.Phase ||
			got.Provider != want.Provider || got.Region != want.Region ||
			got.Access != want.Access || got.Profile != want.Profile ||
			got.Version != want.Version {
			t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
		}
		if !got.CreatedAt.Equal(want.CreatedAt) {
			t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
		}
		if !got.LastReportedAt.IsZero() {
			t.Errorf("LastReportedAt = %v, want zero for a cluster that never reported", got.LastReportedAt)
		}
		if got.Lease != nil {
			t.Errorf("Lease = %+v, want nil on a fresh record", got.Lease)
		}
	})

	t.Run("create rejects a duplicate", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		seed(t, r, clock, "team-alpha")

		_, err := r.Create(context.Background(), testRecord("team-alpha", core.ProviderAzure, clock))
		if !errors.Is(err, ErrAlreadyExists) {
			t.Fatalf("error = %v, want one wrapping ErrAlreadyExists", err)
		}
	})

	t.Run("create rejects an invalid record", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)

		bad := testRecord("team-alpha", core.ProviderAWS, clock)
		bad.Region = ""

		if _, err := r.Create(context.Background(), bad); !errors.Is(err, core.ErrInvalidSpec) {
			t.Fatalf("error = %v, want one wrapping ErrInvalidSpec", err)
		}
	})

	t.Run("update phase advances and bumps version", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		rec := seed(t, r, clock, "team-alpha")

		clock.Advance(time.Minute)
		updated, err := r.UpdatePhase(context.Background(), rec, core.PhaseClusterCreated)
		if err != nil {
			t.Fatalf("UpdatePhase: %v", err)
		}

		if updated.Phase != core.PhaseClusterCreated {
			t.Errorf("Phase = %s, want cluster-created", updated.Phase)
		}
		if updated.Version != rec.Version+1 {
			t.Errorf("Version = %d, want %d", updated.Version, rec.Version+1)
		}
		if !updated.UpdatedAt.After(rec.UpdatedAt) {
			t.Errorf("UpdatedAt = %v, want it to advance from %v", updated.UpdatedAt, rec.UpdatedAt)
		}

		stored, err := r.Get(context.Background(), rec.ClusterID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stored.Phase != core.PhaseClusterCreated || stored.Version != updated.Version {
			t.Errorf("stored record = %+v, want the update to be durable", stored)
		}
	})

	// An illegal transition must fail at the storage boundary, not be written
	// and reconciled later.
	t.Run("update phase rejects an illegal transition", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		rec := seed(t, r, clock, "team-alpha")

		_, err := r.UpdatePhase(context.Background(), rec, core.PhaseReady)
		if !errors.Is(err, core.ErrInvalidTransition) {
			t.Fatalf("error = %v, want one wrapping ErrInvalidTransition", err)
		}

		stored, err := r.Get(context.Background(), rec.ClusterID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stored.Phase != core.PhasePending || stored.Version != rec.Version {
			t.Errorf("record changed despite a rejected transition: %+v", stored)
		}
	})

	t.Run("update phase rejects a stale version", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		rec := seed(t, r, clock, "team-alpha")

		// Someone else advances the record first.
		if _, err := r.UpdatePhase(context.Background(), rec, core.PhaseClusterCreated); err != nil {
			t.Fatalf("first update: %v", err)
		}

		// The stale caller's write must lose rather than overwrite.
		_, err := r.UpdatePhase(context.Background(), rec, core.PhaseClusterCreated)
		if !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("error = %v, want one wrapping ErrVersionConflict", err)
		}
	})

	t.Run("update phase on a missing cluster", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)

		rec := testRecord("absent-cluster", core.ProviderAWS, clock)
		_, err := r.UpdatePhase(context.Background(), rec, core.PhaseClusterCreated)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want one wrapping ErrNotFound", err)
		}
	})

	t.Run("self transition is a no-op that succeeds", func(t *testing.T) {
		// The orchestrator re-writes its current phase on retry; that must not
		// be an error, or retry and first run would need separate code paths.
		clock := newFakeClock()
		r := newRegistry(t, clock)
		rec := seed(t, r, clock, "team-alpha")

		updated, err := r.UpdatePhase(context.Background(), rec, core.PhasePending)
		if err != nil {
			t.Fatalf("self transition: %v", err)
		}
		if updated.Phase != core.PhasePending {
			t.Errorf("Phase = %s, want pending", updated.Phase)
		}
	})

	t.Run("teardown is reachable from a half-provisioned cluster", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		rec := seed(t, r, clock, "team-alpha")

		rec, err := r.UpdatePhase(context.Background(), rec, core.PhaseClusterCreated)
		if err != nil {
			t.Fatalf("advancing: %v", err)
		}
		if _, err := r.UpdatePhase(context.Background(), rec, core.PhaseDecommissioning); err != nil {
			t.Fatalf("decommissioning a half-provisioned cluster: %v", err)
		}
	})

	t.Run("touch records a report without bumping version", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		rec := seed(t, r, clock, "team-alpha")

		reportedAt := clock.Now().Add(2 * time.Minute)
		if err := r.Touch(context.Background(), rec.ClusterID, reportedAt); err != nil {
			t.Fatalf("Touch: %v", err)
		}

		stored, err := r.Get(context.Background(), rec.ClusterID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !stored.LastReportedAt.Equal(reportedAt) {
			t.Errorf("LastReportedAt = %v, want %v", stored.LastReportedAt, reportedAt)
		}
		// Heartbeats arrive constantly; if they bumped the version they would
		// invalidate every in-flight provisioning write.
		if stored.Version != rec.Version {
			t.Errorf("Version = %d, want it unchanged at %d", stored.Version, rec.Version)
		}
	})

	t.Run("touch on a missing cluster", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)

		err := r.Touch(context.Background(), "absent-cluster", clock.Now())
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want one wrapping ErrNotFound", err)
		}
	})

	t.Run("RecordOIDCIssuer sets the issuer without bumping version", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		rec := seed(t, r, clock, "team-alpha")

		const issuer = "https://oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"
		if err := r.RecordOIDCIssuer(context.Background(), rec.ClusterID, issuer); err != nil {
			t.Fatalf("RecordOIDCIssuer: %v", err)
		}

		stored, err := r.Get(context.Background(), rec.ClusterID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stored.OIDCIssuer != issuer {
			t.Errorf("OIDCIssuer = %q, want %q", stored.OIDCIssuer, issuer)
		}
		if stored.Version != rec.Version {
			t.Errorf("Version = %d, want it unchanged at %d", stored.Version, rec.Version)
		}
	})

	t.Run("RecordOIDCIssuer on a missing cluster", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)

		err := r.RecordOIDCIssuer(context.Background(), "absent-cluster", "https://issuer.example.com")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want one wrapping ErrNotFound", err)
		}
	})

	t.Run("list filters", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		ctx := context.Background()

		for _, spec := range []struct {
			id       core.ClusterID
			provider core.Provider
		}{
			{"aws-one", core.ProviderAWS},
			{"aws-two", core.ProviderAWS},
			{"gcp-one", core.ProviderGCP},
		} {
			if _, err := r.Create(ctx, testRecord(spec.id, spec.provider, clock)); err != nil {
				t.Fatalf("seeding %s: %v", spec.id, err)
			}
		}

		// Advance one AWS cluster so a phase filter is meaningful.
		awsOne, err := r.Get(ctx, "aws-one")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if _, err := r.UpdatePhase(ctx, awsOne, core.PhaseClusterCreated); err != nil {
			t.Fatalf("advancing aws-one: %v", err)
		}

		for name, tc := range map[string]struct {
			filter Filter
			want   int
		}{
			"everything":        {Filter{}, 3},
			"by provider":       {Filter{Provider: core.ProviderAWS}, 2},
			"by provider+phase": {Filter{Provider: core.ProviderAWS, Phase: core.PhasePending}, 1},
			"by phase":          {Filter{Phase: core.PhasePending}, 2},
			"no matching phase": {Filter{Phase: core.PhaseReady}, 0},
			"unknown provider":  {Filter{Provider: core.ProviderAzure}, 0},
		} {
			t.Run(name, func(t *testing.T) {
				got, err := r.List(ctx, tc.filter)
				if err != nil {
					t.Fatalf("List: %v", err)
				}
				if len(got) != tc.want {
					t.Errorf("got %d records, want %d: %+v", len(got), tc.want, got)
				}
			})
		}
	})

	runLeaseContract(t, newRegistry)
}

func runLeaseContract(t *testing.T, newRegistry factory) {
	t.Helper()

	const ttl = 5 * time.Minute

	t.Run("acquire on a free cluster", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		rec := seed(t, r, clock, "team-alpha")

		lease, err := r.AcquireLease(context.Background(), rec.ClusterID, "runner-a", ttl)
		if err != nil {
			t.Fatalf("AcquireLease: %v", err)
		}
		if lease.Holder != "runner-a" {
			t.Errorf("Holder = %q, want runner-a", lease.Holder)
		}
		if !lease.ExpiresAt.After(clock.Now()) {
			t.Errorf("ExpiresAt = %v, want a time after now", lease.ExpiresAt)
		}

		stored, err := r.Get(context.Background(), rec.ClusterID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !stored.Held(clock.Now()) {
			t.Error("record does not report the lease as held")
		}
	})

	// The core guarantee: a second concurrent apply must be refused.
	t.Run("acquire is refused while held by another", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		rec := seed(t, r, clock, "team-alpha")

		if _, err := r.AcquireLease(context.Background(), rec.ClusterID, "runner-a", ttl); err != nil {
			t.Fatalf("first acquire: %v", err)
		}

		_, err := r.AcquireLease(context.Background(), rec.ClusterID, "runner-b", ttl)
		if !errors.Is(err, ErrLeaseHeld) {
			t.Fatalf("error = %v, want one wrapping ErrLeaseHeld", err)
		}
	})

	t.Run("re-acquiring your own lease succeeds", func(t *testing.T) {
		// Makes a retried apply idempotent rather than self-blocking.
		clock := newFakeClock()
		r := newRegistry(t, clock)
		rec := seed(t, r, clock, "team-alpha")

		if _, err := r.AcquireLease(context.Background(), rec.ClusterID, "runner-a", ttl); err != nil {
			t.Fatalf("first acquire: %v", err)
		}
		if _, err := r.AcquireLease(context.Background(), rec.ClusterID, "runner-a", ttl); err != nil {
			t.Fatalf("re-acquire by the same holder: %v", err)
		}
	})

	// A crashed run must not wedge a cluster forever.
	t.Run("an expired lease can be taken over", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		rec := seed(t, r, clock, "team-alpha")

		if _, err := r.AcquireLease(context.Background(), rec.ClusterID, "crashed-runner", ttl); err != nil {
			t.Fatalf("first acquire: %v", err)
		}

		clock.Advance(ttl + time.Second)

		lease, err := r.AcquireLease(context.Background(), rec.ClusterID, "runner-b", ttl)
		if err != nil {
			t.Fatalf("taking over an expired lease: %v", err)
		}
		if lease.Holder != "runner-b" {
			t.Errorf("Holder = %q, want runner-b", lease.Holder)
		}
	})

	t.Run("renew extends a held lease", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		rec := seed(t, r, clock, "team-alpha")

		first, err := r.AcquireLease(context.Background(), rec.ClusterID, "runner-a", ttl)
		if err != nil {
			t.Fatalf("AcquireLease: %v", err)
		}

		clock.Advance(time.Minute)
		renewed, err := r.RenewLease(context.Background(), rec.ClusterID, "runner-a", ttl)
		if err != nil {
			t.Fatalf("RenewLease: %v", err)
		}
		if !renewed.ExpiresAt.After(first.ExpiresAt) {
			t.Errorf("renewed expiry %v does not extend %v", renewed.ExpiresAt, first.ExpiresAt)
		}
	})

	t.Run("renew fails for a non-holder", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		rec := seed(t, r, clock, "team-alpha")

		if _, err := r.AcquireLease(context.Background(), rec.ClusterID, "runner-a", ttl); err != nil {
			t.Fatalf("AcquireLease: %v", err)
		}
		if _, err := r.RenewLease(context.Background(), rec.ClusterID, "runner-b", ttl); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("error = %v, want one wrapping ErrLeaseLost", err)
		}
	})

	// Renewing an expired lease must fail: another holder may already own it,
	// and silently re-acquiring would defeat the lock.
	t.Run("renew fails once expired", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		rec := seed(t, r, clock, "team-alpha")

		if _, err := r.AcquireLease(context.Background(), rec.ClusterID, "runner-a", ttl); err != nil {
			t.Fatalf("AcquireLease: %v", err)
		}

		clock.Advance(ttl + time.Second)

		if _, err := r.RenewLease(context.Background(), rec.ClusterID, "runner-a", ttl); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("error = %v, want one wrapping ErrLeaseLost", err)
		}
	})

	t.Run("release frees the cluster", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		rec := seed(t, r, clock, "team-alpha")

		if _, err := r.AcquireLease(context.Background(), rec.ClusterID, "runner-a", ttl); err != nil {
			t.Fatalf("AcquireLease: %v", err)
		}
		if err := r.ReleaseLease(context.Background(), rec.ClusterID, "runner-a"); err != nil {
			t.Fatalf("ReleaseLease: %v", err)
		}

		stored, err := r.Get(context.Background(), rec.ClusterID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stored.Held(clock.Now()) {
			t.Error("cluster still reports a held lease after release")
		}
		if _, err := r.AcquireLease(context.Background(), rec.ClusterID, "runner-b", ttl); err != nil {
			t.Fatalf("acquiring after release: %v", err)
		}
	})

	t.Run("release fails for a non-holder", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		rec := seed(t, r, clock, "team-alpha")

		if _, err := r.AcquireLease(context.Background(), rec.ClusterID, "runner-a", ttl); err != nil {
			t.Fatalf("AcquireLease: %v", err)
		}
		if err := r.ReleaseLease(context.Background(), rec.ClusterID, "runner-b"); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("error = %v, want one wrapping ErrLeaseLost", err)
		}
	})

	t.Run("lease operations on a missing cluster", func(t *testing.T) {
		clock := newFakeClock()
		r := newRegistry(t, clock)
		ctx := context.Background()

		if _, err := r.AcquireLease(ctx, "absent-cluster", "runner-a", ttl); !errors.Is(err, ErrNotFound) {
			t.Errorf("AcquireLease error = %v, want ErrNotFound", err)
		}
		if _, err := r.RenewLease(ctx, "absent-cluster", "runner-a", ttl); !errors.Is(err, ErrNotFound) {
			t.Errorf("RenewLease error = %v, want ErrNotFound", err)
		}
		if err := r.ReleaseLease(ctx, "absent-cluster", "runner-a"); !errors.Is(err, ErrNotFound) {
			t.Errorf("ReleaseLease error = %v, want ErrNotFound", err)
		}
	})

	// The milestone's acceptance criterion, run with real goroutine contention.
	// A sequential simulation of this would pass against a broken lock.
	t.Run("concurrent acquisition elects exactly one holder", func(t *testing.T) {
		const contenders = 16

		clock := newFakeClock()
		r := newRegistry(t, clock)
		rec := seed(t, r, clock, "team-alpha")

		var (
			start   sync.WaitGroup
			done    sync.WaitGroup
			mu      sync.Mutex
			winners []string
		)
		start.Add(1)

		for i := range contenders {
			done.Add(1)
			go func() {
				defer done.Done()

				holder := fmt.Sprintf("runner-%02d", i)
				start.Wait() // release every goroutine at once

				if _, err := r.AcquireLease(context.Background(), rec.ClusterID, holder, ttl); err == nil {
					mu.Lock()
					winners = append(winners, holder)
					mu.Unlock()
				} else if !errors.Is(err, ErrLeaseHeld) {
					t.Errorf("%s: unexpected error %v, want ErrLeaseHeld", holder, err)
				}
			}()
		}

		start.Done()
		done.Wait()

		if len(winners) != 1 {
			t.Fatalf("%d holders acquired the lease (%v), want exactly 1", len(winners), winners)
		}

		stored, err := r.Get(context.Background(), rec.ClusterID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stored.Lease == nil || stored.Lease.Holder != winners[0] {
			t.Errorf("stored lease = %+v, want it held by the winner %s", stored.Lease, winners[0])
		}
	})
}
