package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/GitOpsHub/kubespin/internal/core"
)

// schemaDDL creates the cluster registry table and its provider/phase index,
// idempotently, so a fresh database is ready on first connect without a
// separate migration step. It only ever adds — a run against an
// already-provisioned database is a no-op.
const schemaDDL = `
CREATE TABLE IF NOT EXISTS fleet_registry (
	cluster_id        TEXT PRIMARY KEY,
	phase             TEXT NOT NULL,
	provider          TEXT NOT NULL,
	region            TEXT NOT NULL,
	access            TEXT NOT NULL,
	size              TEXT NOT NULL DEFAULT '',
	version           BIGINT NOT NULL,
	created_at        TIMESTAMPTZ NOT NULL,
	updated_at        TIMESTAMPTZ NOT NULL,
	lease_holder      TEXT,
	lease_expires_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS fleet_registry_provider_phase_idx ON fleet_registry (provider, phase);
ALTER TABLE fleet_registry ADD COLUMN IF NOT EXISTS size TEXT NOT NULL DEFAULT '';
ALTER TABLE fleet_registry DROP COLUMN IF EXISTS profile_name;
ALTER TABLE fleet_registry DROP COLUMN IF EXISTS profile_version;
ALTER TABLE fleet_registry DROP COLUMN IF EXISTS oidc_issuer;
ALTER TABLE fleet_registry DROP COLUMN IF EXISTS last_reported_at;
ALTER TABLE fleet_registry DROP COLUMN IF EXISTS findings;
ALTER TABLE fleet_registry DROP COLUMN IF EXISTS findings_at;
`

// argoCDDetailsDDL creates the cluster_argocd_details table, a child of
// fleet_registry holding one row per cluster's Argo CD connection details.
// provider/region are denormalized from the parent so ad hoc queries don't
// need a join.
const argoCDDetailsDDL = `
CREATE TABLE IF NOT EXISTS cluster_argocd_details (
	cluster_id       TEXT PRIMARY KEY REFERENCES fleet_registry(cluster_id) ON DELETE CASCADE,
	provider         TEXT NOT NULL,
	region           TEXT NOT NULL,
	kube_context     TEXT NOT NULL,
	argocd_endpoint  TEXT NOT NULL,
	argocd_username  TEXT NOT NULL,
	argocd_password  TEXT NOT NULL,
	captured_at      TIMESTAMPTZ NOT NULL,
	updated_at       TIMESTAMPTZ NOT NULL
);
`

// selectColumns is shared by every read (Get, List, and UpdatePhase's
// RETURNING) so a column can't drift between them.
const selectColumns = `
	cluster_id, phase, provider, region, access, size,
	version, created_at, updated_at, lease_holder, lease_expires_at
`

// Postgres is the production Registry, backed by a Postgres database.
type Postgres struct {
	db          *sql.DB
	now         func() time.Time
	logger      *slog.Logger
	retryPolicy RetryPolicy

	// skipMigrations leaves the schema untouched on connect (see
	// WithoutMigrations).
	skipMigrations bool
}

// Option configures a Postgres registry client.
type Option func(*Postgres)

// WithLogger sets the logger. Registry logging is diagnostic detail — the
// commands do their own user-facing reporting — so it is Debug except for a
// lease conflict, which is the race the lease exists to catch.
func WithLogger(logger *slog.Logger) Option {
	return func(p *Postgres) {
		if logger != nil {
			p.logger = logger
		}
	}
}

// WithRetryPolicy replaces the retry policy every call is made under. The
// default (DefaultRetryPolicy) rides out a brief outage; a caller with its
// own deadline discipline can shorten or disable it.
func WithRetryPolicy(policy RetryPolicy) Option {
	return func(p *Postgres) {
		p.retryPolicy = policy
	}
}

// WithoutMigrations connects without running the schema DDL, for a caller
// that must not write to the database at all — a dry run, which promises to
// be strictly read-only. Against a database the DDL has never run on, reads
// then report ErrNotFound rather than failing on the missing table.
func WithoutMigrations() Option {
	return func(p *Postgres) {
		p.skipMigrations = true
	}
}

// WithConnectionPool configures max open/idle connections and connection lifetime.
func WithConnectionPool(maxOpen, maxIdle int, maxLifetime time.Duration) Option {
	return func(p *Postgres) {
		if maxOpen > 0 {
			p.db.SetMaxOpenConns(maxOpen)
		}
		if maxIdle > 0 {
			p.db.SetMaxIdleConns(maxIdle)
		}
		if maxLifetime > 0 {
			p.db.SetConnMaxLifetime(maxLifetime)
		}
	}
}

// NewPostgres opens a connection pool against dsn, verifies it, and
// idempotently ensures the cluster registry table and its index exist.
func NewPostgres(ctx context.Context, dsn string, opts ...Option) (*Postgres, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres connection: %w", err)
	}

	// Default connection pool bounds to protect Postgres from connection exhaustion.
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(15 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}

	p := &Postgres{db: db, now: time.Now, logger: slog.Default()}
	for _, opt := range opts {
		opt(p)
	}

	if p.skipMigrations {
		return p, nil
	}
	if _, err := db.ExecContext(ctx, schemaDDL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating cluster registry schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, argoCDDetailsDDL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating cluster_argocd_details schema: %w", err)
	}

	return p, nil
}

// log returns the client's logger, defaulting when the struct was built
// directly (as the package's own tests do) rather than through NewPostgres.
func (p *Postgres) log() *slog.Logger {
	if p.logger == nil {
		return slog.Default()
	}
	return p.logger
}

// Get returns a cluster's record.
func (p *Postgres) Get(ctx context.Context, id core.ClusterID) (Record, error) {
	var rec Record
	_, err := p.retry(ctx, "get", func(ctx context.Context) error {
		row := p.db.QueryRowContext(ctx, `SELECT `+selectColumns+` FROM fleet_registry WHERE cluster_id = $1`, id.String())
		var err error
		rec, err = scanRecord(row)
		return err
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isUndefinedTable(err) {
			return Record{}, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return Record{}, fmt.Errorf("getting cluster %s: %w", id, err)
	}
	p.log().Debug("Read Registry Record", "cluster", id, "phase", rec.Phase, "version", rec.Version)
	return rec, nil
}

// Create registers a new cluster.
func (p *Postgres) Create(ctx context.Context, rec Record) (Record, error) {
	if err := rec.Validate(); err != nil {
		return Record{}, err
	}
	if rec.Version == 0 {
		rec.Version = 1
	}

	var inserted int64
	attempts, err := p.retry(ctx, "create", func(ctx context.Context) error {
		res, err := p.db.ExecContext(ctx, `
		INSERT INTO fleet_registry (
			cluster_id, phase, provider, region, access, size,
			version, created_at, updated_at, lease_holder, lease_expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (cluster_id) DO NOTHING`,
			rec.ClusterID.String(), rec.Phase.String(), rec.Provider.String(), rec.Region, rec.Access.String(),
			rec.Size.String(), rec.Version,
			rec.CreatedAt.UTC(), rec.UpdatedAt.UTC(), leaseHolder(rec.Lease), leaseExpiry(rec.Lease))
		if err != nil {
			return fmt.Errorf("creating cluster %s: %w", rec.ClusterID, err)
		}
		if inserted, err = res.RowsAffected(); err != nil {
			return fmt.Errorf("creating cluster %s: %w", rec.ClusterID, err)
		}
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	if inserted == 0 {
		// A retried insert that finds the row already there may well have put
		// it there itself, on an attempt whose reply was lost. Claiming
		// ErrAlreadyExists then would fail an apply that in fact succeeded, so
		// the row is read back: ours if it still carries exactly what this
		// call wrote.
		if attempts > 1 && p.matchesOurInsert(ctx, rec) {
			p.log().Debug("Create Landed On Earlier Attempt", "cluster", rec.ClusterID)
			return rec, nil
		}
		return Record{}, fmt.Errorf("%w: %s", ErrAlreadyExists, rec.ClusterID)
	}
	p.log().Debug("Created Registry Record", "cluster", rec.ClusterID, "phase", rec.Phase, "provider", rec.Provider)
	return rec, nil
}

// matchesOurInsert reports whether the stored record is the one this Create
// call wrote — same starting phase and version, no lease taken since. Used
// only to disambiguate a retried insert from a genuine duplicate; a record
// that has moved on belongs to somebody else's run.
func (p *Postgres) matchesOurInsert(ctx context.Context, rec Record) bool {
	stored, err := p.Get(ctx, rec.ClusterID)
	if err != nil {
		return false
	}
	return stored.Phase == rec.Phase && stored.Version == rec.Version &&
		stored.Provider == rec.Provider && stored.Region == rec.Region
}

// UpdatePhase advances a cluster to its next phase.
func (p *Postgres) UpdatePhase(ctx context.Context, rec Record, to core.Phase) (Record, error) {
	// Rejected here rather than at the storage layer's mercy: an illegal
	// transition must never reach the table.
	if err := core.ValidateTransition(rec.Phase, to); err != nil {
		return Record{}, fmt.Errorf("advancing %s: %w", rec.ClusterID, err)
	}

	var updated Record
	attempts, err := p.retry(ctx, "update phase", func(ctx context.Context) error {
		row := p.db.QueryRowContext(ctx, `
		UPDATE fleet_registry
		SET phase = $1, version = version + 1, updated_at = $2
		WHERE cluster_id = $3 AND phase = $4 AND version = $5
		RETURNING `+selectColumns,
			to.String(), p.now().UTC(), rec.ClusterID.String(), rec.Phase.String(), rec.Version)
		var err error
		updated, err = scanRecord(row)
		return err
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Same reasoning as Create: a conditional update that matched
			// nothing, after an attempt whose reply was lost, may be this
			// call's own earlier write. A record already sitting at exactly
			// the phase and version this write would have produced is that
			// write, not somebody else's.
			if attempts > 1 {
				if current, getErr := p.Get(ctx, rec.ClusterID); getErr == nil &&
					current.Phase == to && current.Version == rec.Version+1 {
					p.log().Debug("Phase Transition Landed On Earlier Attempt",
						"cluster", rec.ClusterID, "to", to, "version", current.Version)
					return current, nil
				}
			}
			// Distinguishes not-found from a genuine version conflict, the same
			// two-case split Dynamo's ReturnValuesOnConditionCheckFailure gave for
			// free — here it costs a second read.
			if _, getErr := p.Get(ctx, rec.ClusterID); errors.Is(getErr, ErrNotFound) {
				return Record{}, fmt.Errorf("%w: %s", ErrNotFound, rec.ClusterID)
			}
			return Record{}, fmt.Errorf("%w: %s expected phase %s version %d",
				ErrVersionConflict, rec.ClusterID, rec.Phase, rec.Version)
		}
		return Record{}, fmt.Errorf("updating phase for %s: %w", rec.ClusterID, err)
	}
	p.log().Debug("Recorded Phase Transition",
		"cluster", rec.ClusterID, "from", rec.Phase, "to", to, "version", updated.Version)
	return updated, nil
}

// RecordArgoCDAccess upserts a cluster's Argo CD connection details.
// captured_at is set only by the INSERT branch, so it never moves once
// written; updated_at bumps on every call.
func (p *Postgres) RecordArgoCDAccess(ctx context.Context, id core.ClusterID, access ArgoCDAccess) error {
	now := p.now().UTC()
	var res sql.Result
	_, err := p.retry(ctx, "record argocd access", func(ctx context.Context) error {
		var err error
		res, err = p.db.ExecContext(ctx, `
		INSERT INTO cluster_argocd_details (
			cluster_id, provider, region, kube_context, argocd_endpoint,
			argocd_username, argocd_password, captured_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
		ON CONFLICT (cluster_id) DO UPDATE SET
			provider = EXCLUDED.provider,
			region = EXCLUDED.region,
			kube_context = EXCLUDED.kube_context,
			argocd_endpoint = EXCLUDED.argocd_endpoint,
			argocd_username = EXCLUDED.argocd_username,
			argocd_password = EXCLUDED.argocd_password,
			updated_at = EXCLUDED.updated_at`,
			id.String(), access.Provider.String(), access.Region, access.KubeContext,
			access.Endpoint, access.Username, access.Password, now)
		if err != nil {
			return fmt.Errorf("recording argocd access for %s: %w", id, err)
		}
		return nil
	})
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return fmt.Errorf("recording argocd access for %s: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	p.log().Debug("Recorded Argo CD Access", "cluster", id, "endpoint", access.Endpoint)
	return nil
}

// GetArgoCDAccess returns a cluster's recorded Argo CD access details.
func (p *Postgres) GetArgoCDAccess(ctx context.Context, id core.ClusterID) (ArgoCDAccess, error) {
	var (
		provider, region, kubeContext, endpoint, username, password string
	)
	_, err := p.retry(ctx, "get argocd access", func(ctx context.Context) error {
		row := p.db.QueryRowContext(ctx, `
		SELECT provider, region, kube_context, argocd_endpoint, argocd_username, argocd_password
		FROM cluster_argocd_details WHERE cluster_id = $1`, id.String())
		return row.Scan(&provider, &region, &kubeContext, &endpoint, &username, &password)
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isUndefinedTable(err) {
			return ArgoCDAccess{}, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return ArgoCDAccess{}, fmt.Errorf("getting argocd access for %s: %w", id, err)
	}
	return ArgoCDAccess{
		Provider:    core.Provider(provider),
		Region:      region,
		KubeContext: kubeContext,
		Endpoint:    endpoint,
		Username:    username,
		Password:    password,
	}, nil
}

// isForeignKeyViolation reports whether err is a Postgres foreign-key
// constraint violation (SQLSTATE 23503) — the shape RecordArgoCDAccess gets
// when the referenced fleet_registry row doesn't exist.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// isUndefinedTable reports whether err is Postgres's undefined_table (SQLSTATE
// 42P01): a read against a database the schema DDL has never run on, which
// only a WithoutMigrations client can reach. No table means no record.
func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

// Delete removes a cluster's record. The cluster_argocd_details row goes with
// it through that table's ON DELETE CASCADE, so there is one statement here
// rather than two that could half-fail.
//
// A record that is already gone is a success, not ErrNotFound: delete is
// resumable, and the second run of a teardown whose first run got this far
// must converge rather than fail.
func (p *Postgres) Delete(ctx context.Context, id core.ClusterID) error {
	if _, err := p.retry(ctx, "delete", func(ctx context.Context) error {
		if _, err := p.db.ExecContext(ctx, `DELETE FROM fleet_registry WHERE cluster_id = $1`, id.String()); err != nil {
			return fmt.Errorf("deleting cluster %s: %w", id, err)
		}
		return nil
	}); err != nil {
		return err
	}
	p.log().Debug("Deleted Registry Record", "cluster", id)
	return nil
}

// List returns records matching filter. Postgres reads are always
// consistent, so — unlike the eventually-consistent DynamoDB scan/GSI query
// this replaced — there is no separate index-vs-scan path to choose between
// or page through: one query, filtered by whichever of Provider/Phase are set,
// served by the (provider, phase) index when both are.
func (p *Postgres) List(ctx context.Context, filter Filter) ([]Record, error) {
	var records []Record

	// The whole cursor — query, iterate, close — lives inside one attempt.
	// retry cancels an attempt's context as soon as its function returns, and
	// a *sql.Rows read after that cancellation fails with "context canceled"
	// on the first scan, so returning the rows to be drained outside would
	// break every List against a perfectly healthy database.
	_, err := p.retry(ctx, "list", func(ctx context.Context) error {
		rows, err := p.db.QueryContext(ctx, `
		SELECT `+selectColumns+` FROM fleet_registry
		WHERE ($1 = '' OR provider = $1) AND ($2 = '' OR phase = $2)
		ORDER BY cluster_id`,
			filter.Provider.String(), filter.Phase.String())
		if err != nil {
			return fmt.Errorf("listing registry: %w", err)
		}
		defer func() { _ = rows.Close() }()

		// Reset per attempt: a retry must not append to what a half-drained
		// earlier attempt already collected.
		records = nil
		for rows.Next() {
			rec, scanErr := scanRecord(rows)
			if scanErr != nil {
				return fmt.Errorf("listing registry: %w", scanErr)
			}
			records = append(records, rec)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("listing registry: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// AcquireLease claims a cluster for holder.
func (p *Postgres) AcquireLease(ctx context.Context, id core.ClusterID, holder string, ttl time.Duration) (Lease, error) {
	if holder == "" {
		return Lease{}, fmt.Errorf("%w: lease holder is required", core.ErrInvalidSpec)
	}

	now := p.now().UTC()
	lease := Lease{Holder: holder, ExpiresAt: now.Add(ttl)}

	// Free, expired, or already ours. The expiry comparison is what makes a
	// crashed run self-heal instead of wedging the cluster forever. <= matches
	// Lease.Expired()'s !now.Before(expiresAt) exactly, so "expired" means the
	// same instant here as it does everywhere else that reasons about a lease.
	var n int64
	if _, err := p.retry(ctx, "acquire lease", func(ctx context.Context) error {
		res, err := p.db.ExecContext(ctx, `
		UPDATE fleet_registry
		SET lease_holder = $1, lease_expires_at = $2
		WHERE cluster_id = $3 AND (lease_holder IS NULL OR lease_expires_at <= $4 OR lease_holder = $1)`,
			holder, lease.ExpiresAt, id.String(), now)
		if err != nil {
			return fmt.Errorf("acquiring lease on %s: %w", id, err)
		}
		if n, err = res.RowsAffected(); err != nil {
			return fmt.Errorf("acquiring lease on %s: %w", id, err)
		}
		return nil
	}); err != nil {
		return Lease{}, err
	}
	if n == 0 {
		conflict := p.leaseConflict(ctx, id, ErrLeaseHeld)
		if errors.Is(conflict, ErrLeaseHeld) {
			// The exact race the lease exists to catch: another apply is
			// already provisioning this cluster.
			p.log().Warn("Lease Held By Another Run", "cluster", id, "holder", holder)
		}
		return Lease{}, conflict
	}
	p.log().Debug("Acquired Lease", "cluster", id, "holder", holder, "expiresAt", lease.ExpiresAt)
	return lease, nil
}

// RenewLease extends a lease the caller still holds.
func (p *Postgres) RenewLease(ctx context.Context, id core.ClusterID, holder string, ttl time.Duration) (Lease, error) {
	lease := Lease{Holder: holder, ExpiresAt: p.now().UTC().Add(ttl)}

	// Conditional on the holder alone, deliberately not on the lease still
	// being unexpired. "Another holder may already own it" is exactly what
	// lease_holder <> $3 says: taking over an expired lease (AcquireLease)
	// overwrites the holder, so a row that still names us cannot be held by
	// anybody else, whatever the clock says. Requiring an unexpired lease
	// here instead meant a run that lost contact with Postgres for longer
	// than the TTL killed itself over a lease nobody had taken — and the
	// single statement below stays atomic, so a genuine concurrent takeover
	// still wins the race and this call still reports the loss.
	var n int64
	if _, err := p.retry(ctx, "renew lease", func(ctx context.Context) error {
		res, err := p.db.ExecContext(ctx, `
		UPDATE fleet_registry
		SET lease_expires_at = $1
		WHERE cluster_id = $2 AND lease_holder = $3`,
			lease.ExpiresAt, id.String(), holder)
		if err != nil {
			return fmt.Errorf("renewing lease on %s: %w", id, err)
		}
		if n, err = res.RowsAffected(); err != nil {
			return fmt.Errorf("renewing lease on %s: %w", id, err)
		}
		return nil
	}); err != nil {
		return Lease{}, err
	}
	if n == 0 {
		return Lease{}, p.leaseConflict(ctx, id, ErrLeaseLost)
	}
	p.log().Debug("Renewed Lease", "cluster", id, "holder", holder, "expiresAt", lease.ExpiresAt)
	return lease, nil
}

// ReleaseLease drops a lease the caller holds.
func (p *Postgres) ReleaseLease(ctx context.Context, id core.ClusterID, holder string) error {
	var n int64
	if _, err := p.retry(ctx, "release lease", func(ctx context.Context) error {
		res, err := p.db.ExecContext(ctx, `
		UPDATE fleet_registry
		SET lease_holder = NULL, lease_expires_at = NULL
		WHERE cluster_id = $1 AND lease_holder = $2`,
			id.String(), holder)
		if err != nil {
			return fmt.Errorf("releasing lease on %s: %w", id, err)
		}
		if n, err = res.RowsAffected(); err != nil {
			return fmt.Errorf("releasing lease on %s: %w", id, err)
		}
		return nil
	}); err != nil {
		return err
	}
	if n == 0 {
		return p.leaseConflict(ctx, id, ErrLeaseLost)
	}
	p.log().Debug("Released Lease", "cluster", id, "holder", holder)
	return nil
}

// leaseConflict distinguishes "no such cluster" from a genuine lease
// conflict with a follow-up read, mirroring the item DynamoDB returned
// alongside its failed condition for free.
func (p *Postgres) leaseConflict(ctx context.Context, id core.ClusterID, sentinel error) error {
	rec, err := p.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("checking lease conflict for %s: %w", id, err)
	}
	if rec.Lease != nil && rec.Lease.Holder != "" {
		return fmt.Errorf("%w: %s is held by %s", sentinel, id, rec.Lease.Holder)
	}
	return fmt.Errorf("%w: %s", sentinel, id)
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so scanRecord
// serves Get/UpdatePhase (one row) and List (many) alike.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRecord(s rowScanner) (Record, error) {
	var (
		clusterID, phase, provider, region, access, size string
		version                                          int64
		leaseExpiresAt                                   sql.NullTime
		createdAt, updatedAt                             time.Time
		leaseHolder                                      sql.NullString
	)

	err := s.Scan(
		&clusterID, &phase, &provider, &region, &access, &size,
		&version, &createdAt, &updatedAt, &leaseHolder, &leaseExpiresAt,
	)
	if err != nil {
		return Record{}, fmt.Errorf("scanning record: %w", err)
	}

	rec := Record{
		ClusterID: core.ClusterID(clusterID),
		Phase:     core.Phase(phase),
		Provider:  core.Provider(provider),
		Region:    region,
		Access:    core.Access(access),
		Size:      core.ClusterSize(size),
		Version:   version,
		CreatedAt: createdAt.UTC(),
		UpdatedAt: updatedAt.UTC(),
	}

	if leaseHolder.Valid && leaseHolder.String != "" {
		if !leaseExpiresAt.Valid {
			return Record{}, fmt.Errorf("record %s has a lease holder but no expiry", clusterID)
		}
		rec.Lease = &Lease{Holder: leaseHolder.String, ExpiresAt: leaseExpiresAt.Time.UTC()}
	}

	return rec, nil
}

func leaseHolder(l *Lease) any {
	if l == nil {
		return nil
	}
	return l.Holder
}

func leaseExpiry(l *Lease) any {
	if l == nil {
		return nil
	}
	return l.ExpiresAt.UTC()
}
