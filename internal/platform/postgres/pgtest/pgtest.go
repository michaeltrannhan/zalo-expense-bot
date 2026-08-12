//go:build integration

// Package pgtest provides a real PostgreSQL database for integration tests.
// Tests run only with `-tags integration` and TEST_DATABASE_URL set; the
// helper migrates the schema and truncates user-linked tables between tests
// while preserving global seeds (categories, merchants, aliases).
package pgtest

import (
	"context"
	"io/fs"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zl-expese-bot/db"
	"zl-expese-bot/internal/platform/migrate"
)

var truncatedTables = []string{
	"pending_actions",
	"scheduled_summary_preferences",
	"outbound_messages",
	"queue_jobs",
	"usage_counters",
	"insights",
	"user_merchant_rules",
	"corrections",
	"predictions",
	"extracted_fields",
	"transactions",
	"receipt_processing_attempts",
	"receipt_documents",
	"provider_messages",
	"user_identities",
	"users",
	"merchant_aliases",
	"merchants",
	"categories",
}

// NewPool connects, migrates and truncates. It fails the test on any error.
func NewPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := testDatabaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("pgtest: parse url: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgtest: connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// `go test ./...` runs packages in parallel, but every integration
	// package intentionally points at the same disposable database. Serialize
	// setup + test lifetime across processes so one package cannot truncate
	// another package's fixtures mid-assertion.
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("pgtest: acquire isolation lock connection: %v", err)
	}
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock(891234567)`); err != nil {
		lockConn.Release()
		t.Fatalf("pgtest: acquire isolation lock: %v", err)
	}
	t.Cleanup(func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = lockConn.Exec(unlockCtx, `SELECT pg_advisory_unlock(891234567)`)
		lockConn.Release()
	})

	if _, err := migrate.Up(ctx, pool, db.MigrationsFS, "migrations"); err != nil {
		t.Fatalf("pgtest: migrate: %v", err)
	}
	Truncate(t, pool)
	return pool
}

// Truncate removes all rows from user-linked tables, keeping seed data.
func Truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, table := range truncatedTables {
		if _, err := pool.Exec(ctx, "TRUNCATE "+table+" CASCADE"); err != nil {
			t.Fatalf("pgtest: truncate %s: %v", table, err)
		}
	}
	seed, err := fs.ReadFile(db.MigrationsFS, "migrations/0002_seed_categories.up.sql")
	if err != nil {
		t.Fatalf("pgtest: read global seeds: %v", err)
	}
	if _, err := pool.Exec(ctx, string(seed)); err != nil {
		t.Fatalf("pgtest: restore global seeds: %v", err)
	}
}
