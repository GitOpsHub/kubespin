package registry

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A WithoutMigrations client (a dry run) reading a database the schema DDL has
// never run on gets undefined_table back. That must read as "no record", and
// must not be retried as though the database were merely unavailable.
func TestIsUndefinedTable(t *testing.T) {
	undefined := fmt.Errorf("getting cluster: %w", &pgconn.PgError{Code: "42P01"})

	if !isUndefinedTable(undefined) {
		t.Errorf("isUndefinedTable(wrapped 42P01) = false, want true")
	}
	if isTransient(undefined) {
		t.Errorf("isTransient(42P01) = true, want false: a missing table does not appear on retry")
	}
	for name, err := range map[string]error{
		"foreign key": &pgconn.PgError{Code: "23503"},
		"plain":       fmt.Errorf("boom"),
		"nil":         nil,
	} {
		if isUndefinedTable(err) {
			t.Errorf("isUndefinedTable(%s) = true, want false", name)
		}
	}
}

func TestWithoutMigrations(t *testing.T) {
	p := &Postgres{}
	WithoutMigrations()(p)
	if !p.skipMigrations {
		t.Fatal("WithoutMigrations did not set skipMigrations")
	}
}
