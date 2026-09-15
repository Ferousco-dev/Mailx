package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Mirrors internal/database/testdb_test.go's isolated-schema pattern (see
// that file's doc for why); duplicated here for the same reason
// internal/api's copy is — no shared unexported test helper across
// packages.
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

func newTestDB(t testing.TB) *database.DB {
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

	b := make([]byte, 8)
	_, _ = rand.Read(b)
	schema := "mailx_auth_test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		admin.Close()
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(dropCtx, `DROP SCHEMA "`+schema+`" CASCADE`)
		admin.Close()
	})

	dsn := baseTestDSN(t) + "&search_path=" + schema
	db, err := database.Open(ctx, database.Config{DSN: dsn, MaxConns: 4})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return db
}

func newTestTenant(t testing.TB, db *database.DB) database.Tenant {
	t.Helper()
	tenant, err := db.CreateTenant(context.Background(), "test-tenant")
	if err != nil {
		t.Fatal(err)
	}
	return tenant
}
