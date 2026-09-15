package database

import (
	"context"
	"testing"
	"time"
)

func TestOpenAndPing(t *testing.T) {
	db := newTestDB(t)
	if err := db.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := (Config{}).normalized(); err == nil {
		t.Fatal("expected error for empty DSN")
	}
	if _, err := (Config{DSN: "x", MaxConns: -1}).normalized(); err == nil {
		t.Fatal("expected error for negative MaxConns")
	}
	if _, err := (Config{DSN: "x", MinConns: 5, MaxConns: 1}).normalized(); err == nil {
		t.Fatal("expected error for MinConns > MaxConns")
	}
	if _, err := (Config{DSN: "x", MaxConnLifetime: -1 * time.Second}).normalized(); err == nil {
		t.Fatal("expected error for negative lifetime")
	}
	c, err := (Config{DSN: "x"}).normalized()
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxConns != defaultMaxConns {
		t.Fatalf("expected default MaxConns, got %d", c.MaxConns)
	}
}

func TestConfigStringNeverLeaksDSN(t *testing.T) {
	c := Config{DSN: "postgres://user:supersecret@host/db"}
	s := c.String()
	if contains(s, "supersecret") || contains(s, "postgres://") {
		t.Fatalf("Config.String leaked DSN: %s", s)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := newTestDB(t) // already migrated once by newTestDB
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate call must be a safe no-op: %v", err)
	}
}

func TestMigrateDownThenUpAgain(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Schema must exist before down.
	tenant := newTestTenant(t, db)
	_ = tenant

	// Revert every applied migration, one at a time (newest first), not
	// just the single latest — the schema has grown past one migration.
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for range migrations {
		if err := db.MigrateDownOne(ctx); err != nil {
			t.Fatalf("migrate down: %v", err)
		}
	}
	// Table should be gone now.
	_, err = db.pool.Exec(ctx, `SELECT 1 FROM tenants LIMIT 1`)
	if err == nil {
		t.Fatal("expected tenants table to be dropped after reverting all migrations")
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate up after down: %v", err)
	}
	// Table should exist again, empty (down dropped the CASCADE'd data).
	var count int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&count); err != nil {
		t.Fatalf("tenants table not usable after re-migrating up: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected empty tenants table after down+up, got %d rows", count)
	}
}

func TestMigrateDownOneOnEmptyDatabaseIsNoOp(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := db.MigrateDownOne(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateDownOne(ctx); err != nil {
		t.Fatalf("second down-one on already-empty schema must be a safe no-op: %v", err)
	}
}

func TestPingContextCancellation(t *testing.T) {
	db := newTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := db.Ping(ctx); err == nil {
		t.Fatal("expected error for already-canceled context")
	}
}

func TestOpenRejectsUnreachableDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := Open(ctx, Config{DSN: "postgres://nouser@127.0.0.1:1/nodb?sslmode=disable&connect_timeout=1"})
	if err == nil {
		t.Fatal("expected error opening an unreachable database")
	}
}
