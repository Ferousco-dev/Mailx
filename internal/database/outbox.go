package database

import (
	"context"
	"fmt"
	"time"
)

// OutboxItem is one durable "must eventually reach the queue" record.
type OutboxItem struct {
	MessageID   string
	TenantID    string
	AvailableAt time.Time
}

// maxFairTenants bounds how many tenants one dispatch batch considers, so a
// batch never reads more than maxFairTenants*limit index entries.
const maxFairTenants = 200

// fairOutboxSQL takes $1 now, $2 limit, $3 the tenant cap.
const fairOutboxSQL = `
		WITH RECURSIVE tenants(tenant_id) AS (
			(SELECT tenant_id FROM outbox
			  WHERE dispatched_at IS NULL AND available_at <= $1
			  ORDER BY tenant_id LIMIT 1)
			UNION ALL
			SELECT (SELECT o.tenant_id FROM outbox o
			         WHERE o.dispatched_at IS NULL AND o.available_at <= $1 AND o.tenant_id > t.tenant_id
			         ORDER BY o.tenant_id LIMIT 1)
			FROM tenants t WHERE t.tenant_id IS NOT NULL
		), capped AS (
			SELECT tenant_id FROM tenants WHERE tenant_id IS NOT NULL LIMIT $3
		), ranked AS (
			SELECT o.message_id, o.tenant_id, o.available_at,
			       row_number() OVER (PARTITION BY o.tenant_id ORDER BY o.available_at) AS rn
			FROM capped c
			CROSS JOIN LATERAL (
				SELECT message_id, tenant_id, available_at FROM outbox
				WHERE tenant_id = c.tenant_id AND dispatched_at IS NULL AND available_at <= $1
				ORDER BY available_at LIMIT $2
			) o
		)
		SELECT message_id, tenant_id, available_at FROM ranked
		ORDER BY rn, available_at
		LIMIT $2`

// ListPendingOutbox returns up to limit due, undispatched rows, round-robin
// across tenants: every tenant with due work gets its oldest row first, then
// every tenant's second row, and so on. A tenant with a huge backlog therefore
// cannot push another tenant's few messages out of the batch. Within one tenant
// order is still available_at.
//
// Tenants are discovered with a recursive loose index scan over
// idx_outbox_pending_tenant (one index probe per tenant, not a scan of every
// row). Correctness under several dispatchers does not depend on row locks:
// Enqueue is duplicate-safe by JobID and MarkOutboxDispatched is a guarded
// idempotent no-op the second time. (v0.31 removed FOR UPDATE SKIP LOCKED: each
// read was its own implicit transaction, so the lock was released immediately.)
func (db *DB) ListPendingOutbox(ctx context.Context, now time.Time, limit int) ([]OutboxItem, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.pool.Query(ctx, fairOutboxSQL,
		now, limit, maxFairTenants,
	)
	if err != nil {
		return nil, fmt.Errorf("database: list pending outbox: %w", normalizeErr(err))
	}
	defer rows.Close()

	var out []OutboxItem
	for rows.Next() {
		var item OutboxItem
		if err := rows.Scan(&item.MessageID, &item.TenantID, &item.AvailableAt); err != nil {
			return nil, fmt.Errorf("database: scan outbox item: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// MarkOutboxDispatched records that messageID has been hand off to the
// queue. Called only after a successful (or duplicate-safe no-op) Enqueue,
// so re-running dispatch after a crash before this call is always safe.
func (db *DB) MarkOutboxDispatched(ctx context.Context, messageID string) error {
	tag, err := db.pool.Exec(ctx,
		`UPDATE outbox SET dispatched_at = now() WHERE message_id = $1 AND dispatched_at IS NULL`,
		messageID,
	)
	if err != nil {
		return fmt.Errorf("database: mark outbox dispatched: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
