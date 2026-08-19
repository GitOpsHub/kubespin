package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/GitOpsHub/kubespin/internal/core"
)

// schemaDDL creates the Fleet Registry table and its provider/phase index,
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
	profile_name      TEXT NOT NULL,
	profile_version   TEXT NOT NULL,
	oidc_issuer       TEXT NOT NULL DEFAULT '',
	version           BIGINT NOT NULL,
	last_reported_at  TIMESTAMPTZ,
	findings          JSONB,
	findings_at       TIMESTAMPTZ,
	created_at        TIMESTAMPTZ NOT NULL,
	updated_at        TIMESTAMPTZ NOT NULL,
	lease_holder      TEXT,
	lease_expires_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS fleet_registry_provider_phase_idx ON fleet_registry (provider, phase);
`

// selectColumns is shared by every read (Get, List, and UpdatePhase's
// RETURNING) so a column can't drift between them.
const selectColumns = `
	cluster_id, phase, provider, region, access, profile_name, profile_version,
	oidc_issuer, version, last_reported_at, findings, findings_at, created_at,
	updated_at, lease_holder, lease_expires_at
`

// Postgres is the production Registry, backed by a Postgres database.
type Postgres struct {
	db     *sql.DB
	now    func() time.Time
	logger *slog.Logger
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

// NewPostgres opens a connection pool against dsn, verifies it, and
// idempotently ensures the fleet_registry table and its index exist.
func NewPostgres(ctx context.Context, dsn string, opts ...Option) (*Postgres, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres connection: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if _, err := db.ExecContext(ctx, schemaDDL); err != nil {
		return nil, fmt.Errorf("migrating fleet registry schema: %w", err)
	}

	p := &Postgres{db: db, now: time.Now, logger: slog.Default()}
	for _, opt := range opts {
		opt(p)
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
	row := p.db.QueryRowContext(ctx, `SELECT `+selectColumns+` FROM fleet_registry WHERE cluster_id = $1`, id.String())
	rec, err := scanRecord(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record{}, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return Record{}, fmt.Errorf("getting cluster %s: %w", id, err)
	}
	p.log().Debug("read registry record", "cluster", id, "phase", rec.Phase, "version", rec.Version)
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

	findings, err := findingsJSON(rec)
	if err != nil {
		return Record{}, fmt.Errorf("encoding findings for %s: %w", rec.ClusterID, err)
	}

	res, err := p.db.ExecContext(ctx, `
		INSERT INTO fleet_registry (
			cluster_id, phase, provider, region, access, profile_name, profile_version,
			oidc_issuer, version, last_reported_at, findings, findings_at, created_at,
			updated_at, lease_holder, lease_expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (cluster_id) DO NOTHING`,
		rec.ClusterID.String(), rec.Phase.String(), rec.Provider.String(), rec.Region, rec.Access.String(),
		rec.Profile.Name, rec.Profile.Version, rec.OIDCIssuer, rec.Version,
		nullTime(rec.LastReportedAt), findings, nullTime(rec.FindingsAt),
		rec.CreatedAt.UTC(), rec.UpdatedAt.UTC(), leaseHolder(rec.Lease), leaseExpiry(rec.Lease))
	if err != nil {
		return Record{}, fmt.Errorf("creating cluster %s: %w", rec.ClusterID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Record{}, fmt.Errorf("creating cluster %s: %w", rec.ClusterID, err)
	}
	if n == 0 {
		return Record{}, fmt.Errorf("%w: %s", ErrAlreadyExists, rec.ClusterID)
	}
	p.log().Debug("created registry record", "cluster", rec.ClusterID, "phase", rec.Phase, "provider", rec.Provider)
	return rec, nil
}

// UpdatePhase advances a cluster to its next phase.
func (p *Postgres) UpdatePhase(ctx context.Context, rec Record, to core.Phase) (Record, error) {
	// Rejected here rather than at the storage layer's mercy: an illegal
	// transition must never reach the table.
	if err := core.ValidateTransition(rec.Phase, to); err != nil {
		return Record{}, fmt.Errorf("advancing %s: %w", rec.ClusterID, err)
	}

	row := p.db.QueryRowContext(ctx, `
		UPDATE fleet_registry
		SET phase = $1, version = version + 1, updated_at = $2
		WHERE cluster_id = $3 AND phase = $4 AND version = $5
		RETURNING `+selectColumns,
		to.String(), p.now().UTC(), rec.ClusterID.String(), rec.Phase.String(), rec.Version)

	updated, err := scanRecord(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
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
	p.log().Debug("recorded phase transition",
		"cluster", rec.ClusterID, "from", rec.Phase, "to", to, "version", updated.Version)
	return updated, nil
}

// Touch records a status report.
func (p *Postgres) Touch(ctx context.Context, id core.ClusterID, at time.Time) error {
	err := p.execNoVersionCheck(ctx, id,
		`UPDATE fleet_registry SET last_reported_at = $1 WHERE cluster_id = $2`,
		at.UTC(), id.String())
	if err != nil {
		return err
	}
	p.log().Debug("recorded status report", "cluster", id, "at", at)
	return nil
}

// RecordOIDCIssuer sets the cluster's workload identity issuer.
func (p *Postgres) RecordOIDCIssuer(ctx context.Context, id core.ClusterID, issuer string) error {
	err := p.execNoVersionCheck(ctx, id,
		`UPDATE fleet_registry SET oidc_issuer = $1 WHERE cluster_id = $2`,
		issuer, id.String())
	if err != nil {
		return err
	}
	p.log().Debug("recorded OIDC issuer", "cluster", id, "issuer", issuer)
	return nil
}

// RecordFindings sets the cluster's most recent audit findings.
func (p *Postgres) RecordFindings(ctx context.Context, id core.ClusterID, findings []string, at time.Time) error {
	encoded, err := json.Marshal(findings)
	if err != nil {
		return fmt.Errorf("encoding findings for %s: %w", id, err)
	}

	err = p.execNoVersionCheck(ctx, id,
		`UPDATE fleet_registry SET findings = $1, findings_at = $2 WHERE cluster_id = $3`,
		encoded, at.UTC(), id.String())
	if err != nil {
		return err
	}
	p.log().Debug("recorded audit findings", "cluster", id, "findings", len(findings), "at", at)
	return nil
}

// execNoVersionCheck runs an update conditioned only on the row existing —
// not on Version, the way UpdatePhase is. Touch, RecordOIDCIssuer, and
// RecordFindings all write observational metadata that arrives independently
// of phase transitions and must not contend with them, so none of them
// assert a version.
func (p *Postgres) execNoVersionCheck(ctx context.Context, id core.ClusterID, query string, args ...any) error {
	res, err := p.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("updating %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("updating %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

// List returns records matching filter. Postgres reads are always
// consistent, so — unlike the eventually-consistent DynamoDB scan/GSI query
// this replaced — there is no separate index-vs-scan path to choose between
// or page through: one query, filtered by whichever of Provider/Phase are set,
// served by the (provider, phase) index when both are.
func (p *Postgres) List(ctx context.Context, filter Filter) ([]Record, error) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT `+selectColumns+` FROM fleet_registry
		WHERE ($1 = '' OR provider = $1) AND ($2 = '' OR phase = $2)
		ORDER BY cluster_id`,
		filter.Provider.String(), filter.Phase.String())
	if err != nil {
		return nil, fmt.Errorf("listing registry: %w", err)
	}
	defer rows.Close()

	var records []Record
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("listing registry: %w", err)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing registry: %w", err)
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
	res, err := p.db.ExecContext(ctx, `
		UPDATE fleet_registry
		SET lease_holder = $1, lease_expires_at = $2
		WHERE cluster_id = $3 AND (lease_holder IS NULL OR lease_expires_at <= $4 OR lease_holder = $1)`,
		holder, lease.ExpiresAt, id.String(), now)
	if err != nil {
		return Lease{}, fmt.Errorf("acquiring lease on %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Lease{}, fmt.Errorf("acquiring lease on %s: %w", id, err)
	}
	if n == 0 {
		conflict := p.leaseConflict(ctx, id, ErrLeaseHeld)
		if errors.Is(conflict, ErrLeaseHeld) {
			// The exact race the lease exists to catch: another apply is
			// already provisioning this cluster.
			p.log().Warn("lease acquisition conflicted with another run", "cluster", id, "holder", holder)
		}
		return Lease{}, conflict
	}
	p.log().Debug("acquired lease", "cluster", id, "holder", holder, "expires_at", lease.ExpiresAt)
	return lease, nil
}

// RenewLease extends a lease the caller still holds.
func (p *Postgres) RenewLease(ctx context.Context, id core.ClusterID, holder string, ttl time.Duration) (Lease, error) {
	now := p.now().UTC()
	lease := Lease{Holder: holder, ExpiresAt: now.Add(ttl)}

	// Strictly greater than now: an expired lease cannot be renewed, because
	// another holder may already own it.
	res, err := p.db.ExecContext(ctx, `
		UPDATE fleet_registry
		SET lease_expires_at = $1
		WHERE cluster_id = $2 AND lease_holder = $3 AND lease_expires_at > $4`,
		lease.ExpiresAt, id.String(), holder, now)
	if err != nil {
		return Lease{}, fmt.Errorf("renewing lease on %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Lease{}, fmt.Errorf("renewing lease on %s: %w", id, err)
	}
	if n == 0 {
		return Lease{}, p.leaseConflict(ctx, id, ErrLeaseLost)
	}
	p.log().Debug("renewed lease", "cluster", id, "holder", holder, "expires_at", lease.ExpiresAt)
	return lease, nil
}

// ReleaseLease drops a lease the caller holds.
func (p *Postgres) ReleaseLease(ctx context.Context, id core.ClusterID, holder string) error {
	res, err := p.db.ExecContext(ctx, `
		UPDATE fleet_registry
		SET lease_holder = NULL, lease_expires_at = NULL
		WHERE cluster_id = $1 AND lease_holder = $2`,
		id.String(), holder)
	if err != nil {
		return fmt.Errorf("releasing lease on %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("releasing lease on %s: %w", id, err)
	}
	if n == 0 {
		return p.leaseConflict(ctx, id, ErrLeaseLost)
	}
	p.log().Debug("released lease", "cluster", id, "holder", holder)
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
		clusterID, phase, provider, region, access, profileName, profileVersion, oidcIssuer string
		version                                                                             int64
		lastReportedAt, findingsAt, leaseExpiresAt                                          sql.NullTime
		findingsRaw                                                                         []byte
		createdAt, updatedAt                                                                time.Time
		leaseHolder                                                                         sql.NullString
	)

	err := s.Scan(
		&clusterID, &phase, &provider, &region, &access, &profileName, &profileVersion,
		&oidcIssuer, &version, &lastReportedAt, &findingsRaw, &findingsAt, &createdAt,
		&updatedAt, &leaseHolder, &leaseExpiresAt,
	)
	if err != nil {
		return Record{}, err
	}

	rec := Record{
		ClusterID:  core.ClusterID(clusterID),
		Phase:      core.Phase(phase),
		Provider:   core.Provider(provider),
		Region:     region,
		Access:     core.Access(access),
		Profile:    core.ProfileRef{Name: profileName, Version: profileVersion},
		OIDCIssuer: oidcIssuer,
		Version:    version,
		CreatedAt:  createdAt.UTC(),
		UpdatedAt:  updatedAt.UTC(),
	}

	if lastReportedAt.Valid {
		rec.LastReportedAt = lastReportedAt.Time.UTC()
	}

	// findings_at absent means never audited — distinct from an empty Findings
	// list, which means a clean audit ran.
	if findingsAt.Valid {
		rec.FindingsAt = findingsAt.Time.UTC()
		var findings []string
		if len(findingsRaw) > 0 {
			if err := json.Unmarshal(findingsRaw, &findings); err != nil {
				return Record{}, fmt.Errorf("parsing findings for %s: %w", clusterID, err)
			}
		}
		rec.Findings = findings
	}

	if leaseHolder.Valid && leaseHolder.String != "" {
		if !leaseExpiresAt.Valid {
			return Record{}, fmt.Errorf("record %s has a lease holder but no expiry", clusterID)
		}
		rec.Lease = &Lease{Holder: leaseHolder.String, ExpiresAt: leaseExpiresAt.Time.UTC()}
	}

	return rec, nil
}

// findingsJSON encodes rec's findings for storage, or nil (SQL NULL) when the
// cluster has never been audited — kept distinct from an empty, encoded `[]`,
// which means a clean audit ran.
func findingsJSON(rec Record) (any, error) {
	if rec.FindingsAt.IsZero() {
		return nil, nil
	}
	b, err := json.Marshal(rec.Findings)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// nullTime returns nil (SQL NULL) for a zero time, so "never reported"/"never
// audited" is stored as absent rather than as the zero time itself.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
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
