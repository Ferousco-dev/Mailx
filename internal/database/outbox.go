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

// ListPendingOutbox returns up to limit due, undispatched rows, oldest
// available_at first. Correctness under multiple dispatchers does not
// depend on SKIP LOCKED holding a lock for the whole dispatch cycle (each
// row here is its own implicit transaction) - it comes from Enqueue being
// duplicate-safe by JobID and MarkOutboxDispatched being a guarded,
// idempotent no-op the second time. SKIP LOCKED just avoids two
// dispatchers reading the exact same row in the same instant.
func (db *DB) ListPendingOutbox(ctx context.Context, now time.Time, limit int) ([]OutboxItem, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.pool.Query(ctx, `
		SELECT message_id, tenant_id, available_at
		FROM outbox
		WHERE dispatched_at IS NULL AND available_at <= $1
		ORDER BY available_at
		LIMIT $2
		FOR UPDATE SKIP LOCKED`,
		now, limit,
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
