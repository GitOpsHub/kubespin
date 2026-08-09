package registry

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// Memory is an in-memory Registry.
//
// It exists so that every component built on the registry — the orchestrator
// above all — is testable without credentials or a container. It is not a
// simplified stand-in: it enforces exactly the same conditions as the DynamoDB
// implementation, and both are exercised by the same contract test suite. A
// fake with weaker semantics would let real bugs pass.
type Memory struct {
	mu      sync.Mutex
	records map[core.ClusterID]Record
	now     func() time.Time
}

// MemoryOption configures a Memory registry.
type MemoryOption func(*Memory)

// WithClock replaces the time source, so lease expiry is testable without
// sleeping.
func WithClock(now func() time.Time) MemoryOption {
	return func(m *Memory) { m.now = now }
}

// NewMemory returns an empty in-memory registry.
func NewMemory(opts ...MemoryOption) *Memory {
	m := &Memory{records: map[core.ClusterID]Record{}, now: time.Now}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// clone deep-copies a record so callers cannot mutate stored state through the
// Lease pointer they were handed.
func clone(rec Record) Record {
	if rec.Lease != nil {
		lease := *rec.Lease
		rec.Lease = &lease
	}
	return rec
}

// Get returns a cluster's record.
func (m *Memory) Get(_ context.Context, id core.ClusterID) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, ok := m.records[id]
	if !ok {
		return Record{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return clone(rec), nil
}

// Create registers a new cluster.
func (m *Memory) Create(_ context.Context, rec Record) (Record, error) {
	if err := rec.Validate(); err != nil {
		return Record{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.records[rec.ClusterID]; exists {
		return Record{}, fmt.Errorf("%w: %s", ErrAlreadyExists, rec.ClusterID)
	}

	if rec.Version == 0 {
		rec.Version = 1
	}
	m.records[rec.ClusterID] = clone(rec)
	return clone(rec), nil
}

// UpdatePhase advances a cluster to its next phase.
func (m *Memory) UpdatePhase(_ context.Context, rec Record, to core.Phase) (Record, error) {
	// Validated before touching stored state: an illegal transition must fail
	// at the storage boundary, not be persisted and cleaned up later.
	if err := core.ValidateTransition(rec.Phase, to); err != nil {
		return Record{}, fmt.Errorf("advancing %s: %w", rec.ClusterID, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	stored, ok := m.records[rec.ClusterID]
	if !ok {
		return Record{}, fmt.Errorf("%w: %s", ErrNotFound, rec.ClusterID)
	}
	if stored.Version != rec.Version || stored.Phase != rec.Phase {
		return Record{}, fmt.Errorf("%w: %s is at phase %s version %d, expected %s version %d",
			ErrVersionConflict, rec.ClusterID, stored.Phase, stored.Version, rec.Phase, rec.Version)
	}

	stored.Phase = to
	stored.Version++
	stored.UpdatedAt = m.now()
	m.records[rec.ClusterID] = stored

	return clone(stored), nil
}

// Touch records a status report.
func (m *Memory) Touch(_ context.Context, id core.ClusterID, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored, ok := m.records[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	// No version bump: a heartbeat is not a data change to contend over.
	stored.LastReportedAt = at
	m.records[id] = stored
	return nil
}

// List returns records matching filter, ordered by cluster ID.
func (m *Memory) List(_ context.Context, filter Filter) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []Record
	for _, rec := range m.records {
		if filter.Provider != "" && rec.Provider != filter.Provider {
			continue
		}
		if filter.Phase != "" && rec.Phase != filter.Phase {
			continue
		}
		out = append(out, clone(rec))
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ClusterID < out[j].ClusterID })
	return out, nil
}

// AcquireLease claims a cluster for holder.
func (m *Memory) AcquireLease(_ context.Context, id core.ClusterID, holder string, ttl time.Duration) (Lease, error) {
	if holder == "" {
		return Lease{}, fmt.Errorf("%w: lease holder is required", core.ErrInvalidSpec)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	stored, ok := m.records[id]
	if !ok {
		return Lease{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	now := m.now()
	if stored.Lease != nil && !stored.Lease.Expired(now) && stored.Lease.Holder != holder {
		return Lease{}, fmt.Errorf("%w: %s is held by %s until %s",
			ErrLeaseHeld, id, stored.Lease.Holder, stored.Lease.ExpiresAt.UTC().Format(time.RFC3339))
	}

	lease := Lease{Holder: holder, ExpiresAt: now.Add(ttl)}
	stored.Lease = &lease
	m.records[id] = stored

	return lease, nil
}

// RenewLease extends a lease the caller still holds.
func (m *Memory) RenewLease(_ context.Context, id core.ClusterID, holder string, ttl time.Duration) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored, ok := m.records[id]
	if !ok {
		return Lease{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	now := m.now()
	// An expired lease cannot be renewed: another holder may already have taken
	// it, and silently re-acquiring here would defeat the lock.
	if stored.Lease == nil || stored.Lease.Holder != holder || stored.Lease.Expired(now) {
		return Lease{}, fmt.Errorf("%w: %s", ErrLeaseLost, id)
	}

	lease := Lease{Holder: holder, ExpiresAt: now.Add(ttl)}
	stored.Lease = &lease
	m.records[id] = stored

	return lease, nil
}

// ReleaseLease drops a lease the caller holds.
func (m *Memory) ReleaseLease(_ context.Context, id core.ClusterID, holder string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored, ok := m.records[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if stored.Lease == nil || stored.Lease.Holder != holder {
		return fmt.Errorf("%w: %s", ErrLeaseLost, id)
	}

	stored.Lease = nil
	m.records[id] = stored
	return nil
}
