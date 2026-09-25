// Package database's data-retention surface (v0.46). Messages/recipients/
// delivery_attempts/events are kept indefinitely by default in MailX's
// original schema; this file adds a per-tenant configurable purge of
// message data older than a retention window, defaulting to
// DefaultRetentionDays when a tenant hasn't set one - an unconfigured
// tenant still gets purged on the same schedule as everyone else, which is
// the safer default for a compliance feature (see migration 000025's
// comment).
//
// Scope: purging deletes messages and everything that legitimately belongs
// ONLY to that message (recipients, delivery_attempts, events - all
// ON DELETE CASCADE). It never touches suppressions (a compliance/deny-list
// record with its own independent lifecycle, already ON DELETE SET NULL on
// its message_id - see migration 000013), contacts/audiences (business
// records, not message data), domains, templates, or webhook config.
package database

import (
	"context"
	"errors"
	"fmt"
)

// DefaultRetentionDays applies to any tenant whose retention_days is NULL.
const DefaultRetentionDays = 90

// purgeBatchLimit bounds how many expired messages one PurgeExpiredMessages
// call deletes: an unbounded backlog would otherwise collect an
// arbitrarily large in-memory id slice and hold one long-running
// transaction that competes with live traffic. The hourly ticker calling
// this simply catches up over multiple runs on a large backlog.
const purgeBatchLimit = 1000

// SetTenantRetention sets tenantID's retention window in days, or clears it
// back to DefaultRetentionDays when days is nil. A non-nil days must be
// positive - the database CHECK constraint also enforces this, but
// rejecting here gives a clearer error than a raw constraint violation.
func (db *DB) SetTenantRetention(ctx context.Context, tenantID string, days *int) error {
	if tenantID == "" {
		return errors.New("database: tenant ID is empty")
	}
	if days != nil && *days <= 0 {
		return errors.New("database: retention_days must be positive")
	}
	tag, err := db.pool.Exec(ctx, `UPDATE tenants SET retention_days = $1 WHERE id = $2`, days, tenantID)
	if err != nil {
		return fmt.Errorf("database: set tenant retention: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// PurgeExpiredMessages hard-deletes every message (and, via ON DELETE
// CASCADE, its recipients/delivery_attempts/events) whose created_at is
// older than its tenant's effective retention window
// (COALESCE(retention_days, DefaultRetentionDays) days before now).
//
// deleteDisk is called once per candidate id, BEFORE that id's database row
// is deleted - never after. This ordering matters: this function's
// database.DB has no reference to internal/storage.FileStore (avoiding a
// layering dependency), and previously the caller ran the DB delete first
// and best-effort-deleted disk files after, logging only a warning on
// failure - a crash or disk error after that DB commit permanently orphaned
// raw mail on disk with no DB row left to find it by. Now an id whose
// deleteDisk call fails (or returns a "not found" the caller doesn't treat
// as success) is simply left out of this cycle's DB delete, so its row
// survives to be retried - together with its disk file - on the next purge
// tick. deleteDisk should treat "file already absent" as success.
//
// broadcast_recipients.message_id has no foreign key to messages (it
// predates this feature and was never given one), so a purged message's
// id is explicitly nulled out there first to avoid leaving a dangling
// reference.
func (db *DB) PurgeExpiredMessages(ctx context.Context, deleteDisk func(id string) error) ([]string, error) {
	// Only terminal messages are eligible: 'queued'/'processing'/'retrying'
	// (messages_status_check, migration 000013) mean the message has not
	// finished its delivery lifecycle yet - a scheduled send accepted long
	// before its send_at, or one still being retried, has created_at older
	// than the window but must not be purged before the worker ever loads
	// it. Only 'delivered'/'failed'/'bounced'/'suppressed' are terminal.
	rows, err := db.pool.Query(ctx, `
		SELECT m.id
		FROM messages m
		JOIN tenants t ON t.id = m.tenant_id
		WHERE m.created_at < now() - make_interval(days => COALESCE(t.retention_days, $1))
		  AND m.status NOT IN ('queued', 'processing', 'retrying')
		ORDER BY m.created_at
		LIMIT $2`,
		DefaultRetentionDays, purgeBatchLimit,
	)
	if err != nil {
		return nil, fmt.Errorf("database: select expired messages: %w", normalizeErr(err))
	}
	var candidates []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("database: scan expired message id: %w", err)
		}
		candidates = append(candidates, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	if len(candidates) == 0 {
		return nil, nil
	}

	// Disk cleanup happens BEFORE any database row is deleted (see doc
	// above) - only ids that succeed here are eligible for the DB delete
	// below.
	ids := make([]string, 0, len(candidates))
	for _, id := range candidates {
		if err := deleteDisk(id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("database: begin purge: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `UPDATE broadcast_recipients SET message_id = NULL WHERE message_id = ANY($1)`, ids); err != nil {
		return nil, fmt.Errorf("database: detach broadcast_recipients before purge: %w", normalizeErr(err))
	}
	if _, err := tx.Exec(ctx, `DELETE FROM messages WHERE id = ANY($1)`, ids); err != nil {
		return nil, fmt.Errorf("database: purge messages: %w", normalizeErr(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("database: commit purge: %w", normalizeErr(err))
	}
	return ids, nil
}
