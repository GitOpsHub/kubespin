//go:build integration

package registry

import (
	"context"
	"os"
	"testing"
)

// The registry contract, run against a real Postgres rather than the
// in-memory implementation. The RETURNING/ON CONFLICT semantics the lease and
// optimistic-concurrency logic depend on cannot be proven by a fake alone —
// this is where "the SQL is actually correct" gets tested.
//
// Requires a reachable Postgres database:
//
//	docker run -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:16
//	KUBESPIN_POSTGRES_TEST_DSN=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable make integration
func TestPostgresRegistry(t *testing.T) {
	dsn := os.Getenv("KUBESPIN_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("KUBESPIN_POSTGRES_TEST_DSN is not set; skipping Postgres integration tests")
	}

	runContract(t, func(t *testing.T, clock *fakeClock) Registry {
		t.Helper()

		p, err := NewPostgres(context.Background(), dsn)
		if err != nil {
			t.Fatalf("connecting to postgres: %v", err)
		}
		// Every contract subtest expects a fresh, empty registry — truncate up
		// front rather than relying on the previous factory call's cleanup, so a
		// failed prior run can't leave stale rows behind.
		if _, err := p.db.ExecContext(context.Background(), "TRUNCATE TABLE cluster_argocd_details, fleet_registry"); err != nil {
			t.Fatalf("truncating fleet_registry: %v", err)
		}
		t.Cleanup(func() {
			if err := p.db.Close(); err != nil {
				t.Errorf("closing postgres connection: %v", err)
			}
		})

		p.now = clock.Now
		return p
	})
}
