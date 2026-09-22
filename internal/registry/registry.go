// Package registry is the client for the cluster registry, the single source
// of durable per-cluster state.
//
// Every component reads and writes cluster state through this package rather
// than through raw SDK calls, so the invariants live in one place: phase
// transitions are validated before they can be persisted, writes carry an
// optimistic-concurrency check, and provisioning is serialised by a lease.
package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// Sentinel errors. Callers branch with errors.Is rather than matching messages.
var (
	// ErrNotFound is returned when no record exists for a cluster.
	ErrNotFound = errors.New("cluster not found in registry")
	// ErrAlreadyExists is returned when creating a cluster that is already registered.
	ErrAlreadyExists = errors.New("cluster already exists in registry")
	// ErrVersionConflict means the record changed underneath a read-modify-write.
	ErrVersionConflict = errors.New("registry record was modified concurrently")
	// ErrLeaseHeld means another holder has an unexpired lease on the cluster.
	ErrLeaseHeld = errors.New("cluster lease is held by another holder")
	// ErrLeaseLost means the caller no longer holds the lease it is trying to use.
	ErrLeaseLost = errors.New("cluster lease is no longer held by this holder")
)

// Lease is a time-bounded claim on a cluster, preventing two concurrent
// `apply` runs from provisioning the same cluster at once.
//
// It expires rather than being held indefinitely: a crashed run must not wedge
// a cluster forever, so the claim self-heals once the TTL passes.
type Lease struct {
	Holder    string
	ExpiresAt time.Time
}

// Expired reports whether the lease is no longer valid at now.
func (l Lease) Expired(now time.Time) bool { return !now.Before(l.ExpiresAt) }

// Record is one cluster's row in the registry: the durable half of its state.
// The other half — resolved addons, node pool detail — lives in the cluster's
// own repository. This holds only what apply and delete need to resume.
type Record struct {
	ClusterID core.ClusterID
	Phase     core.Phase

	Provider core.Provider
	Region   string
	Access   core.Access
	Size     core.ClusterSize

	// Version is bumped on every data write and asserted as a condition, so a
	// read-modify-write that raced another writer fails instead of overwriting.
	Version int64

	CreatedAt time.Time
	UpdatedAt time.Time

	// Lease is nil when unheld. An expired lease may still be present until the
	// next acquisition overwrites it.
	Lease *Lease
}

// Held reports whether an unexpired lease exists at now.
func (r Record) Held(now time.Time) bool {
	return r.Lease != nil && !r.Lease.Expired(now)
}

// Validate checks the fields the registry is responsible for.
func (r Record) Validate() error {
	var errs []error
	if err := r.ClusterID.Validate(); err != nil {
		errs = append(errs, err)
	}
	if !r.Phase.Valid() {
		errs = append(errs, fmt.Errorf("%w: phase %q is not a known phase", core.ErrInvalidSpec, r.Phase))
	}
	if !r.Provider.Valid() {
		errs = append(errs, fmt.Errorf("%w: provider %q is not supported", core.ErrInvalidSpec, r.Provider))
	}
	if r.Region == "" {
		errs = append(errs, fmt.Errorf("%w: region is required", core.ErrInvalidSpec))
	}
	if !r.Access.Valid() {
		errs = append(errs, fmt.Errorf("%w: access %q must be private or public", core.ErrInvalidSpec, r.Access))
	}
	return errors.Join(errs...)
}

// NewRecord builds a pending record from a validated cluster spec.
func NewRecord(spec core.ClusterSpec, now time.Time) Record {
	return Record{
		ClusterID: spec.ID,
		Phase:     core.PhasePending,
		Provider:  spec.Provider,
		Region:    spec.Region,
		Access:    spec.Access,
		Size:      spec.Size,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// ArgoCDAccess is a cluster's Argo CD connection details, captured once the
// cluster reaches PhaseReady and Argo CD's LoadBalancer endpoint resolves.
// It is observational metadata, not part of the phase state machine.
type ArgoCDAccess struct {
	Provider    core.Provider
	Region      string
	KubeContext string
	Endpoint    string // argocd-server LB external IP or hostname, no scheme
	Username    string
	Password    string // plaintext, matching the trust model of KUBESPIN_REGISTRY_DSN
}

// Filter narrows a List. A zero Filter matches every cluster.
type Filter struct {
	// Provider, when set, is served by the provider/phase index rather than a
	// table scan — which is why the index exists from the day the table does.
	Provider core.Provider
	Phase    core.Phase
}

// Registry is the durable store of per-cluster state.
//
// Implementations must enforce three things, because callers depend on them
// rather than re-checking: an illegal phase transition is rejected, a write
// against a stale Version is rejected, and a lease held by someone else cannot
// be taken, renewed, or released.
type Registry interface {
	// Get returns a cluster's record, or ErrNotFound.
	Get(ctx context.Context, id core.ClusterID) (Record, error)

	// Create registers a new cluster, or returns ErrAlreadyExists.
	Create(ctx context.Context, rec Record) (Record, error)

	// UpdatePhase advances rec to the next phase. It fails with
	// ErrInvalidTransition if the move is illegal, or ErrVersionConflict if the
	// stored record no longer matches rec's phase and version.
	UpdatePhase(ctx context.Context, rec Record, to core.Phase) (Record, error)

	// List returns records matching filter.
	List(ctx context.Context, filter Filter) ([]Record, error)

	// AcquireLease claims the cluster for holder until now+ttl. It returns
	// ErrLeaseHeld if another holder's lease is still valid. An expired lease is
	// taken over without ceremony.
	AcquireLease(ctx context.Context, id core.ClusterID, holder string, ttl time.Duration) (Lease, error)

	// RenewLease extends a lease the caller still holds. Renewing an expired
	// lease returns ErrLeaseLost: by then another holder may already own it, and
	// silently re-acquiring would defeat the lock.
	RenewLease(ctx context.Context, id core.ClusterID, holder string, ttl time.Duration) (Lease, error)

	// ReleaseLease drops a lease the caller holds. Releasing one held by someone
	// else returns ErrLeaseLost.
	ReleaseLease(ctx context.Context, id core.ClusterID, holder string) error

	// RecordArgoCDAccess upserts a cluster's Argo CD connection details. It
	// carries no version check — observational metadata, not a phase
	// transition — and re-recording replaces the previous values.
	// Returns ErrNotFound if the cluster has no record.
	RecordArgoCDAccess(ctx context.Context, id core.ClusterID, access ArgoCDAccess) error

	// GetArgoCDAccess returns a cluster's recorded Argo CD access details, or
	// ErrNotFound if none have been captured.
	GetArgoCDAccess(ctx context.Context, id core.ClusterID) (ArgoCDAccess, error)
}
