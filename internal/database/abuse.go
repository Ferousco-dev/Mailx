package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CountTenantQueued returns how many of a tenant's messages are not finished
// (queued or processing), counting at most limit rows so the query cost is
// bounded by the cap it is compared against, never by the size of the backlog.
func (db *DB) CountTenantQueued(ctx context.Context, tenantID string, limit int) (int, error) {
	var n int
	err := db.pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT 1 FROM messages
			WHERE tenant_id = $1 AND status IN ('queued','processing')
			LIMIT $2
		) s`, tenantID, limit).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("database: count tenant queued: %w", normalizeErr(err))
	}
	return n, nil
}

// CountPendingOutbox returns how many outbox rows are waiting to be handed to
// the queue, counting at most limit rows (bounded, an index-only scan of idx_outbox_pending_tenant).
func (db *DB) CountPendingOutbox(ctx context.Context, limit int) (int, error) {
	var n int
	err := db.pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT 1 FROM outbox WHERE dispatched_at IS NULL LIMIT $1
		) s`, limit).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("database: count pending outbox: %w", normalizeErr(err))
	}
	return n, nil
}

// MessageTenant returns the tenant that owns a message. The worker uses it to
// take a per-tenant delivery permit; it never crosses a tenant boundary because
// the message id is the queue's own identifier.
func (db *DB) MessageTenant(ctx context.Context, messageID string) (string, error) {
	var tenant string
	err := db.pool.QueryRow(ctx, `SELECT tenant_id FROM messages WHERE id = $1`, messageID).Scan(&tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("database: message tenant: %w", normalizeErr(err))
	}
	return tenant, nil
}

// ReleaseIdempotencyClaim deletes an in-progress claim this request owns, used
// when the request is refused AFTER claiming (a rate or capacity refusal). The
// fingerprint guard means a claim that was reclaimed by another request is left
// alone, and a completed key is never touched. A refused request must not
// consume its key: the client retries the same key after Retry-After.
func (db *DB) ReleaseIdempotencyClaim(ctx context.Context, tenantID, operation, key, fingerprint string) error {
	_, err := db.pool.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE tenant_id = $1 AND operation = $2 AND idempotency_key = $3
		  AND fingerprint = $4 AND status = 'in_progress'`,
		tenantID, operation, key, fingerprint)
	if err != nil {
		return fmt.Errorf("database: release idempotency claim: %w", normalizeErr(err))
	}
	return nil
}
