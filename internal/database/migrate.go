package database

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

var migrationFileRE = regexp.MustCompile(`^(\d{6})_([a-zA-Z0-9_]+)\.(up|down)\.sql$`)

// migration is one versioned, named, directional schema change.
type migration struct {
	version int
	name    string
	up      string
	down    string
}

// loadMigrations reads every embedded up/down pair, sorted by version. A
// missing down file is rejected — every migration needs a rollback story.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("database: read migrations: %w", err)
	}
	byVersion := map[int]*migration{}
	for _, e := range entries {
		m := migrationFileRE.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("database: unrecognized migration filename %q", e.Name())
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("database: invalid migration version in %q: %w", e.Name(), err)
		}
		content, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("database: read %q: %w", e.Name(), err)
		}
		entry, ok := byVersion[version]
		if !ok {
			entry = &migration{version: version, name: m[2]}
			byVersion[version] = entry
		} else if entry.name != m[2] {
			return nil, fmt.Errorf("database: migration %06d has mismatched names %q and %q", version, entry.name, m[2])
		}
		switch m[3] {
		case "up":
			entry.up = string(content)
		case "down":
			entry.down = string(content)
		}
	}
	out := make([]migration, 0, len(byVersion))
	for _, m := range byVersion {
		if strings.TrimSpace(m.up) == "" {
			return nil, fmt.Errorf("database: migration %06d_%s missing up.sql", m.version, m.name)
		}
		if m.down == "" {
			return nil, fmt.Errorf("database: migration %06d_%s missing down.sql (rollback is required, even if intentionally a no-op)", m.version, m.name)
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

const schemaMigrationsDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);`

// Migrate applies every unapplied migration in order, each in its own
// transaction. Idempotent; safe to call on every startup.
func (db *DB) Migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	if _, err := db.pool.Exec(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("database: ensure schema_migrations: %w", err)
	}
	applied, err := db.appliedVersions(ctx)
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if err := db.applyOne(ctx, m, m.up, "up"); err != nil {
			return err
		}
	}
	return nil
}

// MigrateDownOne reverts the single most-recently-applied migration.
func (db *DB) MigrateDownOne(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	byVersion := make(map[int]migration, len(migrations))
	for _, m := range migrations {
		byVersion[m.version] = m
	}
	applied, err := db.appliedVersions(ctx)
	if err != nil {
		return err
	}
	latest := -1
	for v := range applied {
		if v > latest {
			latest = v
		}
	}
	if latest == -1 {
		return nil // nothing applied; safe no-op
	}
	m, ok := byVersion[latest]
	if !ok {
		return fmt.Errorf("database: applied migration version %d has no corresponding embedded migration", latest)
	}
	return db.applyOne(ctx, m, m.down, "down")
}

func (db *DB) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := db.pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("database: query applied migrations: %w", err)
	}
	defer rows.Close()
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("database: scan applied migration: %w", err)
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

func (db *DB) applyOne(ctx context.Context, m migration, sqlText string, direction string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin migration %06d (%s): %w", m.version, direction, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op if already committed

	if _, err := tx.Exec(ctx, sqlText); err != nil {
		return fmt.Errorf("database: apply migration %06d_%s (%s): %w", m.version, m.name, direction, err)
	}
	if direction == "up" {
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.version, m.name); err != nil {
			return fmt.Errorf("database: record migration %06d: %w", m.version, err)
		}
	} else {
		if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, m.version); err != nil {
			return fmt.Errorf("database: unrecord migration %06d: %w", m.version, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit migration %06d (%s): %w", m.version, direction, err)
	}
	return nil
}
