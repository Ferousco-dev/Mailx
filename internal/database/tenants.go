package database

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Tenant is the ownership root for every other MailX record. v0.15
// implements only what establishes correct ownership boundaries now — no
// API keys, no authentication, no billing. Those are later milestones.
type Tenant struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

// CreateTenant inserts a new tenant and returns its generated ID.
func (db *DB) CreateTenant(ctx context.Context, name string) (Tenant, error) {
	if strings.TrimSpace(name) == "" {
		return Tenant{}, fmt.Errorf("database: tenant name is empty")
	}
	id, err := newID()
	if err != nil {
		return Tenant{}, err
	}
	var t Tenant
	err = db.pool.QueryRow(ctx,
		`INSERT INTO tenants (id, name) VALUES ($1, $2) RETURNING id, name, created_at`,
		id, name,
	).Scan(&t.ID, &t.Name, &t.CreatedAt)
	if err != nil {
		return Tenant{}, normalizeErr(err)
	}
	return t, nil
}

// GetTenant loads one tenant by ID. Returns ErrNotFound if absent.
func (db *DB) GetTenant(ctx context.Context, id string) (Tenant, error) {
	var t Tenant
	err := db.pool.QueryRow(ctx,
		`SELECT id, name, created_at FROM tenants WHERE id = $1`, id,
	).Scan(&t.ID, &t.Name, &t.CreatedAt)
	if err != nil {
		return Tenant{}, normalizeErr(err)
	}
	return t, nil
}
