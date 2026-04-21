// Package testhelp provides shared test plumbing for adapter packages.
// It intentionally depends only on pgx so each adapter can use it without
// pulling in the other adapter's library.
package testhelp

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresURL returns the Postgres URL for adapter tests, or causes the
// test to skip if unavailable. Tests opt in to running against a live
// Postgres by setting QB_POSTGRES_URL in the environment; the Makefile
// sets this for `make test`.
func PostgresURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("QB_POSTGRES_URL")
	if url == "" {
		t.Skip("QB_POSTGRES_URL not set; skipping Postgres-backed adapter test")
	}
	return url
}

// Pool opens a pgxpool connected to the benchmark Postgres. Intended for
// adapter tests that need to inspect the queue tables directly.
func Pool(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

// TruncateAll drops and recreates the public schema to give each test a
// clean slate. Safe because the benchmark Postgres is ephemeral.
func TruncateAll(t *testing.T, url string) {
	t.Helper()
	pool := Pool(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
}
