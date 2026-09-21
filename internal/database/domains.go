package database

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type DomainStatus string

const (
	DomainPending  DomainStatus = "pending"
	DomainVerified DomainStatus = "verified"
)

type Domain struct {
	ID                 string
	TenantID           string
	Name               string
	VerificationStatus DomainStatus
	VerificationToken  string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	VerifiedAt         *time.Time
	LastCheckedAt      *time.Time
	DeletedAt          *time.Time
}

type DomainCursor struct {
	CreatedAt time.Time
	ID        string
}

func scanDomain(row rowScanner) (Domain, error) {
	var d Domain
	err := row.Scan(&d.ID, &d.TenantID, &d.Name, &d.VerificationStatus, &d.VerificationToken,
		&d.CreatedAt, &d.UpdatedAt, &d.VerifiedAt, &d.LastCheckedAt, &d.DeletedAt)
	return d, normalizeErr(err)
}

const domainColumns = `id, tenant_id, name, verification_status, verification_token,
created_at, updated_at, verified_at, last_checked_at, deleted_at`

func (db *DB) CreateDomain(ctx context.Context, tenantID, name, token string) (Domain, error) {
	if tenantID == "" || name == "" || token == "" {
		return Domain{}, errors.New("database: tenant, domain name, and token are required")
	}
	id, err := newID()
	if err != nil {
		return Domain{}, err
	}
	row := db.pool.QueryRow(ctx, `INSERT INTO domains (id, tenant_id, name, verification_token)
		VALUES ($1, $2, $3, $4) RETURNING `+domainColumns, id, tenantID, name, token)
	d, err := scanDomain(row)
	if err != nil {
		return Domain{}, fmt.Errorf("database: create domain: %w", err)
	}
	return d, nil
}

func (db *DB) GetDomain(ctx context.Context, tenantID, id string) (Domain, error) {
	row := db.pool.QueryRow(ctx, `SELECT `+domainColumns+`
		FROM domains WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, id)
	return scanDomain(row)
}

func (db *DB) ListDomains(ctx context.Context, tenantID string, limit int, after *DomainCursor) ([]Domain, error) {
	if tenantID == "" || limit < 1 || limit > 500 {
		return nil, errors.New("database: invalid domain list arguments")
	}
	var afterTime *time.Time
	var afterID *string
	if after != nil {
		afterTime, afterID = &after.CreatedAt, &after.ID
	}
	rows, err := db.pool.Query(ctx, `SELECT `+domainColumns+`
		FROM domains
		WHERE tenant_id = $1 AND deleted_at IS NULL
		  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3))
		ORDER BY created_at DESC, id DESC LIMIT $4`, tenantID, afterTime, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("database: list domains: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []Domain
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, fmt.Errorf("database: scan domain: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (db *DB) RecordDomainCheck(ctx context.Context, tenantID, id string, verified bool, checkedAt time.Time) (Domain, error) {
	status := DomainPending
	if verified {
		status = DomainVerified
	}
	row := db.pool.QueryRow(ctx, `UPDATE domains
		SET verification_status = CASE WHEN $3 THEN 'verified' ELSE verification_status END,
		    verified_at = CASE WHEN $3 THEN COALESCE(verified_at, $4) ELSE verified_at END,
		    last_checked_at = $4, updated_at = $4
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL
		RETURNING `+domainColumns, tenantID, id, verified, checkedAt)
	d, err := scanDomain(row)
	if err != nil {
		return Domain{}, fmt.Errorf("database: record %s domain check: %w", status, err)
	}
	return d, nil
}

func (db *DB) DeleteDomain(ctx context.Context, tenantID, id string, deletedAt time.Time) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin delete domain: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE domains SET deleted_at = $3, updated_at = $3
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, id, deletedAt)
	if err != nil {
		return fmt.Errorf("database: delete domain: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	// A deleted domain must stop signing at once, and its private keys must not
	// survive to be reachable by whoever claims the name next.
	if _, err := tx.Exec(ctx, `DELETE FROM dkim_keys WHERE tenant_id = $1 AND domain_id = $2`, tenantID, id); err != nil {
		return fmt.Errorf("database: delete domain DKIM keys: %w", normalizeErr(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit delete domain: %w", normalizeErr(err))
	}
	return nil
}
