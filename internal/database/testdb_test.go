package database

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Real-PostgreSQL integration tests. MAILX_TEST_DATABASE_URL selects the
// server; it defaults to a local Postgres with no password (matching this
// development environment's peer/trust auth), targeting a pre-created
// "mailx_test" database. Each test gets its own PostgreSQL SCHEMA (never a
// shared one), created fresh and dropped in cleanup, so tests never
// interfere with each other and never require a human to maintain rows.
func baseTestDSN(t testing.TB) string {
	t.Helper()
	if dsn := os.Getenv("MAILX_TEST_DATABASE_URL"); dsn != "" {
		return dsn
	}
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	return fmt.Sprintf("postgres://%s@localhost:5432/mailx_test?sslmode=disable", user)
}

func randomSchemaName(t testing.TB) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "mailx_test_" + hex.EncodeToString(b)
}

// newTestDB creates an isolated schema, opens a *DB scoped to it via
// search_path, migrates it, and registers cleanup to drop the schema and
// close the pool.
func newTestDB(t testing.TB) *DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, baseTestDSN(t))
	if err != nil {
		t.Skipf("no local PostgreSQL available for integration tests: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Skipf("no local PostgreSQL available for integration tests: %v", err)
	}

	schema := randomSchemaName(t)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+quoteIdent(schema)); err != nil {
		admin.Close()
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		_, _ = admin.Exec(dropCtx, `DROP SCHEMA `+quoteIdent(schema)+` CASCADE`)
		admin.Close()
	})

	dsn := baseTestDSN(t) + "&search_path=" + schema
	db, err := Open(ctx, Config{DSN: dsn, MaxConns: 4})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(db.Close)

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return db
}

// quoteIdent is a minimal, deliberately narrow identifier quoter for the
// schema names THIS test file itself generates (hex-suffixed, always safe)
// — it is not a general-purpose SQL-safety mechanism and must never be used
// on any externally supplied value.
func quoteIdent(name string) string {
	return `"` + name + `"`
}

func newTestTenant(t testing.TB, db *DB) Tenant {
	t.Helper()
	tenant, err := db.CreateTenant(context.Background(), "test-tenant")
	if err != nil {
		t.Fatal(err)
	}
	return tenant
}
