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
	"time"

	"github.com/Ferousco-dev/mailx/internal/billing"
)

// DefaultRetentionDays applies to any tenant whose retention_days is NULL
// while plan enforcement is OFF (self-hosted). With enforcement on, a NULL
// retention_days falls back to the tenant's plan RetentionDays instead
// (DEC-224); an explicit retention_days always wins.
const DefaultRetentionDays = 90

// DefaultRetentionDaysFor is the effective default window for a tenant on
// planID, honoring whether plan enforcement is enabled.
func (db *DB) DefaultRetentionDaysFor(planID string) int {
	if db.PlanEnforcementEnabled() {
		return billing.PlanFor(planID).RetentionDays
	}
	return DefaultRetentionDays
}

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
// Known, accepted narrow race (not fixed - see risks.md): if deleteDisk
// succeeds for an id but the DB transaction below then fails (a connection
// drop between here and commit), that id's disk file is gone while its row
// survives - a lookup in that window returns a record with no body. This
// is self-healing: the row is still an eligible candidate next cycle,
// deleteDisk is idempotent (an already-absent file is success), so the next
// purge tick completes the DB delete. A truly atomic fix needs a two-phase
// "pending purge" marker column, out of scope for this fix pass - the
// alternative (DB delete before disk delete) was the ORIGINAL bug this
// ordering exists to prevent (permanent orphan, not a self-healing window),
// so this ordering is the deliberately lesser risk.
//
// candidateScanFactor bounds how many EXTRA rows beyond purgeBatchLimit one
// call will page through to work around persistently-failing deletes: a
// message whose deleteDisk keeps failing (e.g. a permissions problem)
// would otherwise be reselected first, in the same oldest-first order,
// every single cycle - starving every other expired message behind it from
// ever being reached. Paging past failures (up to this many rows scanned)
// lets the batch fill with whatever GOOD candidates exist beyond the stuck
// one, while the stuck id itself is simply left for a later cycle (its row
// is untouched, so it is not lost - just deferred).
//
// broadcast_recipients.message_id has no foreign key to messages (it
// predates this feature and was never given one), so a purged message's
// id is explicitly nulled out there first to avoid leaving a dangling
// reference.
const candidateScanFactor = 10

func (db *DB) PurgeExpiredMessages(ctx context.Context, deleteDisk func(id string) error) ([]string, error) {
	ids, err := db.collectPurgeableIDs(ctx, deleteDisk)
	if err != nil {
		return nil, err
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

// collectPurgeableIDs pages through expired-message candidates (oldest
// first) via (created_at, id) keyset pagination, calling deleteDisk on
// each and keeping only the ones that succeed - see PurgeExpiredMessages's
// doc for why a persistently-failing id must not block progress on the
// rest of the backlog. Stops once purgeBatchLimit successes are collected
// or candidateScanFactor*purgeBatchLimit rows have been scanned, whichever
// comes first (the latter bounds one call's work even if almost every
// candidate in a very large backlog is currently failing).
func (db *DB) collectPurgeableIDs(ctx context.Context, deleteDisk func(id string) error) ([]string, error) {
	const pageSize = 200
	maxScanned := purgeBatchLimit * candidateScanFactor

	ids := make([]string, 0, purgeBatchLimit)
	var afterCreatedAt time.Time
	var afterID string
	scanned := 0

	for len(ids) < purgeBatchLimit && scanned < maxScanned {
		rows, err := db.pool.Query(ctx, `
			SELECT m.id, m.created_at
			FROM messages m
			JOIN tenants t ON t.id = m.tenant_id
			WHERE m.created_at < now() - make_interval(days => COALESCE(t.retention_days,
			      CASE WHEN $5 THEN CASE t.plan WHEN 'free' THEN $6::int WHEN 'plus' THEN $7::int ELSE $8::int END ELSE $1 END))
			  AND m.status NOT IN ('queued', 'processing', 'retrying')
			  AND (m.created_at, m.id) > ($2, $3)
			ORDER BY m.created_at, m.id
			LIMIT $4`,
			DefaultRetentionDays, afterCreatedAt, afterID, pageSize, db.PlanEnforcementEnabled(),
			billing.PlanFor(billing.PlanFree).RetentionDays, billing.PlanFor(billing.PlanPlus).RetentionDays, billing.PlanFor(billing.PlanPro).RetentionDays,
		)
		if err != nil {
			return nil, fmt.Errorf("database: select expired messages: %w", normalizeErr(err))
		}
		type candidate struct {
			id        string
			createdAt time.Time
		}
		var page []candidate
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.id, &c.createdAt); err != nil {
				rows.Close()
				return nil, fmt.Errorf("database: scan expired message id: %w", err)
			}
			page = append(page, c)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		rows.Close()

		if len(page) == 0 {
			break // no more candidates at all
		}
		for _, c := range page {
			scanned++
			if deleteDisk(c.id) == nil {
				ids = append(ids, c.id)
				if len(ids) >= purgeBatchLimit {
					break
				}
			}
			afterCreatedAt, afterID = c.createdAt, c.id
		}
	}
	return ids, nil
}
