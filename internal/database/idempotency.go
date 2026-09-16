package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// IdempotencyStatus mirrors the CHECK constraint on idempotency_keys.status.
type IdempotencyStatus string

const (
	IdempotencyInProgress IdempotencyStatus = "in_progress"
	IdempotencyCompleted  IdempotencyStatus = "completed"
)

// IdempotencyRecord is one durable idempotency claim/result.
type IdempotencyRecord struct {
	TenantID       string
	Operation      string
	IdempotencyKey string
	Fingerprint    string
	Status         IdempotencyStatus
	ResourceID     *string
	CreatedAt      time.Time
	CompletedAt    *time.Time
	ExpiresAt      time.Time
}

// ClaimIdempotencyKey is the ONLY place two concurrent requests racing on
// the same (tenant, operation, key) are arbitrated — by PostgreSQL, not a
// process-local lock, so it is correct even across multiple MailX
// instances. It tries, in order:
//
//  1. INSERT ... ON CONFLICT DO NOTHING — wins outright if nobody holds
//     this key yet. Exactly one concurrent caller can ever succeed here.
//  2. If that hit a conflict, an UPDATE that only matches a row stuck at
//     status='in_progress' older than staleCutoff — reclaims a claim
//     whose owner crashed before completing (see
//     internal/database.InsertMessage's idempotency-completion doc for
//     what "completing" means). Also atomic: only one racing reclaimer
//     can match the same row.
//  3. Otherwise the key is legitimately owned by someone else (an
//     in-progress request still within its staleness window, or an
//     already-completed one) — the caller gets owned=false and must
//     inspect the returned existing record itself.
func (db *DB) ClaimIdempotencyKey(ctx context.Context, tenantID, operation, key, fingerprint string, expiresAt, staleCutoff time.Time) (record IdempotencyRecord, owned bool, err error) {
	row := db.pool.QueryRow(ctx, `
		INSERT INTO idempotency_keys (tenant_id, operation, idempotency_key, fingerprint, status, expires_at)
		VALUES ($1, $2, $3, $4, 'in_progress', $5)
		ON CONFLICT (tenant_id, operation, idempotency_key) DO NOTHING
		RETURNING tenant_id, operation, idempotency_key, fingerprint, status, resource_id, created_at, completed_at, expires_at`,
		tenantID, operation, key, fingerprint, expiresAt,
	)
	rec, ok, scanErr := scanIdempotencyRow(row)
	if scanErr != nil {
		return IdempotencyRecord{}, false, fmt.Errorf("database: claim idempotency key: %w", normalizeErr(scanErr))
	}
	if ok {
		return rec, true, nil
	}

	reclaimRow := db.pool.QueryRow(ctx, `
		UPDATE idempotency_keys
		SET fingerprint = $4, status = 'in_progress', resource_id = NULL, completed_at = NULL,
		    created_at = now(), expires_at = $5
		WHERE tenant_id = $1 AND operation = $2 AND idempotency_key = $3
		  AND status = 'in_progress' AND created_at < $6
		RETURNING tenant_id, operation, idempotency_key, fingerprint, status, resource_id, created_at, completed_at, expires_at`,
		tenantID, operation, key, fingerprint, expiresAt, staleCutoff,
	)
	rec, ok, scanErr = scanIdempotencyRow(reclaimRow)
	if scanErr != nil {
		return IdempotencyRecord{}, false, fmt.Errorf("database: reclaim idempotency key: %w", normalizeErr(scanErr))
	}
	if ok {
		return rec, true, nil
	}

	existing, err := db.GetIdempotencyKey(ctx, tenantID, operation, key)
	if err != nil {
		return IdempotencyRecord{}, false, fmt.Errorf("database: load existing idempotency key after lost claim race: %w", err)
	}
	return existing, false, nil
}

func (db *DB) GetIdempotencyKey(ctx context.Context, tenantID, operation, key string) (IdempotencyRecord, error) {
	row := db.pool.QueryRow(ctx, `
		SELECT tenant_id, operation, idempotency_key, fingerprint, status, resource_id, created_at, completed_at, expires_at
		FROM idempotency_keys WHERE tenant_id = $1 AND operation = $2 AND idempotency_key = $3`,
		tenantID, operation, key,
	)
	rec, ok, err := scanIdempotencyRow(row)
	if err != nil {
		return IdempotencyRecord{}, normalizeErr(err)
	}
	if !ok {
		return IdempotencyRecord{}, ErrNotFound
	}
	return rec, nil
}

// DeleteExpiredIdempotencyKeys removes up to limit rows whose retention
// window has passed, regardless of status — a row still 'in_progress'
// past its own expires_at is abandoned by definition (its guarantee
// window is over), not active work to protect.
func (db *DB) DeleteExpiredIdempotencyKeys(ctx context.Context, now time.Time, limit int) (int64, error) {
	tag, err := db.pool.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE ctid IN (
			SELECT ctid FROM idempotency_keys WHERE expires_at < $1 LIMIT $2
		)`,
		now, limit,
	)
	if err != nil {
		return 0, fmt.Errorf("database: delete expired idempotency keys: %w", normalizeErr(err))
	}
	return tag.RowsAffected(), nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanIdempotencyRow(row rowScanner) (IdempotencyRecord, bool, error) {
	var rec IdempotencyRecord
	err := row.Scan(&rec.TenantID, &rec.Operation, &rec.IdempotencyKey, &rec.Fingerprint,
		&rec.Status, &rec.ResourceID, &rec.CreatedAt, &rec.CompletedAt, &rec.ExpiresAt)
	if err != nil {
		if isNoRows(err) {
			return IdempotencyRecord{}, false, nil
		}
		return IdempotencyRecord{}, false, err
	}
	return rec, true, nil
}
