// Package database is MailX's PostgreSQL persistence foundation. It does
// not store raw MIME/attachment bytes — rows reference a message by the
// same id internal/storage uses on disk. SQL is explicit (pgx), not an ORM.
package database

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("database: not found")
	ErrConflict = errors.New("database: conflict")
)

// Config configures the connection pool. DSN uses PostgreSQL's standard
// connection-string syntax and is never logged (see String).
type Config struct {
	DSN             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

const (
	defaultMaxConns        = 10
	defaultMaxConnLifetime = time.Hour
	defaultMaxConnIdleTime = 30 * time.Minute
)

// String omits DSN so an accidental log/print never leaks credentials.
func (c Config) String() string {
	return fmt.Sprintf("database.Config{MaxConns:%d MinConns:%d MaxConnLifetime:%s MaxConnIdleTime:%s}",
		c.MaxConns, c.MinConns, c.MaxConnLifetime, c.MaxConnIdleTime)
}

func (c Config) normalized() (Config, error) {
	if c.DSN == "" {
		return Config{}, errors.New("database: DSN is empty")
	}
	if c.MaxConns == 0 {
		c.MaxConns = defaultMaxConns
	}
	if c.MaxConns < 0 || c.MinConns < 0 {
		return Config{}, errors.New("database: connection pool sizes must not be negative")
	}
	if c.MinConns > c.MaxConns {
		return Config{}, errors.New("database: MinConns must not exceed MaxConns")
	}
	if c.MaxConnLifetime == 0 {
		c.MaxConnLifetime = defaultMaxConnLifetime
	}
	if c.MaxConnIdleTime == 0 {
		c.MaxConnIdleTime = defaultMaxConnIdleTime
	}
	if c.MaxConnLifetime < 0 || c.MaxConnIdleTime < 0 {
		return Config{}, errors.New("database: connection lifetimes must not be negative")
	}
	return c, nil
}

type DB struct {
	pool *pgxpool.Pool
}

// Open builds a bounded connection pool and verifies connectivity.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	normalized, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	poolCfg, err := pgxpool.ParseConfig(normalized.DSN)
	if err != nil {
		return nil, fmt.Errorf("database: parse config: %w", err)
	}
	poolCfg.MaxConns = normalized.MaxConns
	poolCfg.MinConns = normalized.MinConns
	poolCfg.MaxConnLifetime = normalized.MaxConnLifetime
	poolCfg.MaxConnIdleTime = normalized.MaxConnIdleTime

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("database: create pool: %w", err)
	}
	db := &DB{pool: pool}
	if err := db.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) Ping(ctx context.Context) error {
	if err := db.pool.Ping(ctx); err != nil {
		return fmt.Errorf("database: ping: %w", err)
	}
	return nil
}

func (db *DB) Close() {
	db.pool.Close()
}

// newID matches internal/storage.NewID's style (crypto/rand, 16 bytes,
// hex), generated locally rather than imported to keep this package
// decoupled from storage's specific ID semantics.
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("database: generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// normalizeErr maps pgx/pgconn errors to package sentinels so callers
// never branch on driver-specific values.
func normalizeErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505", "23503": // unique_violation, foreign_key_violation
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
	}
	return err
}
