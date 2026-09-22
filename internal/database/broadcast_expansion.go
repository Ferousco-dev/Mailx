package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ClaimActiveBroadcasts returns up to limit broadcasts still needing
// expansion work (accepted or expanding) that are DUE (send_at is NULL, the
// unchanged v0.36 immediate case, or send_at <= PostgreSQL's own now() — the
// same clock-authority lesson v0.36 learned the hard way: never compare a
// stored instant against a Go-supplied "now"). Oldest first, locked with
// SKIP LOCKED so concurrent poller instances never claim the same broadcast
// in the same tick (tested: two expanders never double-process one
// broadcast). A future-dated broadcast is simply invisible to this query
// until due — no second scheduler, no early activation.
func (db *DB) ClaimActiveBroadcasts(ctx context.Context, limit int) ([]Broadcast, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+broadcastColumns+`
		FROM broadcasts
		WHERE status IN ('accepted','expanding') AND (send_at IS NULL OR send_at <= now())
		ORDER BY created_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("database: claim active broadcasts: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []Broadcast
	for rows.Next() {
		b, err := scanBroadcast(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MarkBroadcastExpanding transitions accepted -> expanding (idempotent: a
// no-op if already expanding) and stamps started_at once.
func (db *DB) MarkBroadcastExpanding(ctx context.Context, id string) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE broadcasts SET status = 'expanding', started_at = COALESCE(started_at, now()), updated_at = now()
		WHERE id = $1 AND status = 'accepted'`, id)
	if err != nil {
		return fmt.Errorf("database: mark broadcast expanding: %w", normalizeErr(err))
	}
	return nil
}

// MarkBroadcastFailed stops all further expansion of this broadcast (it is
// no longer selected by ClaimActiveBroadcasts). Used only for a condition
// affecting the WHOLE broadcast (e.g. its From domain is no longer
// verified) — a single recipient's problem never fails the broadcast (see
// MarkRecipientSuppressed for the per-recipient path).
func (db *DB) MarkBroadcastFailed(ctx context.Context, id, reason string) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE broadcasts SET status = 'failed', failure_reason = $2, completed_at = now(), updated_at = now()
		WHERE id = $1 AND status IN ('accepted','expanding')`, id, reason)
	if err != nil {
		return fmt.Errorf("database: mark broadcast failed: %w", normalizeErr(err))
	}
	return nil
}

// SnapshotBroadcastBatch advances the durable audience scan by AT MOST
// batchSize members: one bounded transaction, never the whole audience (see
// docs/design-v0.36.md "no giant transaction"). Membership rows are
// filtered to audience_snapshot_at, so a member added to the Audience after
// acceptance is never included, however long expansion takes; a member
// REMOVED before its range is scanned is simply absent from this read (a
// documented narrow race, not a correctness bug — see RSK).
//
// Insertion uses ON CONFLICT DO NOTHING on (broadcast_id, contact_id), so
// re-running the SAME cursor range after a crash (before the cursor advance
// committed) creates no duplicate recipient rows — idempotent by
// construction, not by "the crash probably won't happen there".
func (db *DB) SnapshotBroadcastBatch(ctx context.Context, b Broadcast, batchSize int) (advanced int, exhausted bool, err error) {
	if b.SnapshotComplete {
		return 0, true, nil
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("database: begin snapshot batch: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var cursorAt *time.Time
	var cursorID *string
	if err := tx.QueryRow(ctx, `
		SELECT expansion_cursor_created_at, expansion_cursor_contact_id FROM broadcasts WHERE id = $1 FOR UPDATE`,
		b.ID).Scan(&cursorAt, &cursorID); err != nil {
		return 0, false, fmt.Errorf("database: lock broadcast: %w", normalizeErr(err))
	}

	rows, err := tx.Query(ctx, `
		SELECT m.contact_id, m.created_at, c.email, c.name, c.attributes
		FROM audience_members m JOIN contacts c ON c.id = m.contact_id
		WHERE m.tenant_id = $1 AND m.audience_id = $2 AND m.created_at <= $3
		  AND ($4::timestamptz IS NULL OR (m.created_at, m.contact_id) > ($4, $5))
		ORDER BY m.created_at, m.contact_id
		LIMIT $6`, b.TenantID, b.AudienceID, b.AudienceSnapshotAt, cursorAt, cursorID, batchSize)
	if err != nil {
		return 0, false, fmt.Errorf("database: scan audience batch: %w", normalizeErr(err))
	}
	type member struct {
		contactID, email, name string
		createdAt              time.Time
		attrs                  []byte
	}
	var members []member
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.contactID, &m.createdAt, &m.email, &m.name, &m.attrs); err != nil {
			rows.Close()
			return 0, false, fmt.Errorf("database: scan audience member: %w", err)
		}
		members = append(members, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, false, err
	}

	if len(members) > 0 {
		batch := &pgx.Batch{}
		for _, m := range members {
			id, idErr := newID()
			if idErr != nil {
				return 0, false, idErr
			}
			batch.Queue(`
				INSERT INTO broadcast_recipients (id, broadcast_id, tenant_id, contact_id, email, name, attributes)
				VALUES ($1,$2,$3,$4,$5,$6,$7)
				ON CONFLICT (broadcast_id, contact_id) DO NOTHING`,
				id, b.ID, b.TenantID, m.contactID, m.email, m.name, m.attrs)
		}
		results := tx.SendBatch(ctx, batch)
		for range members {
			if _, err := results.Exec(); err != nil {
				_ = results.Close()
				return 0, false, fmt.Errorf("database: insert recipient snapshot: %w", normalizeErr(err))
			}
		}
		if err := results.Close(); err != nil {
			return 0, false, fmt.Errorf("database: close recipient snapshot batch: %w", normalizeErr(err))
		}
		last := members[len(members)-1]
		if _, err := tx.Exec(ctx, `
			UPDATE broadcasts SET expansion_cursor_created_at = $2, expansion_cursor_contact_id = $3, updated_at = now()
			WHERE id = $1`, b.ID, last.createdAt, last.contactID); err != nil {
			return 0, false, fmt.Errorf("database: advance snapshot cursor: %w", normalizeErr(err))
		}
	}

	exhausted = len(members) < batchSize
	if exhausted {
		if _, err := tx.Exec(ctx, `UPDATE broadcasts SET snapshot_complete = true, updated_at = now() WHERE id = $1`, b.ID); err != nil {
			return 0, false, fmt.Errorf("database: mark snapshot complete: %w", normalizeErr(err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("database: commit snapshot batch: %w", normalizeErr(err))
	}
	return len(members), exhausted, nil
}

// recipientClaimLease bounds how long a claimed-but-not-yet-finished
// recipient stays unavailable to other claimants. It must comfortably
// exceed one materialization attempt (render+MIME+DKIM+InsertMessage) so a
// legitimately slow attempt is never reclaimed out from under itself, while
// still being short enough that a crashed claimant's rows become claimable
// again promptly.
const recipientClaimLease = 5 * time.Minute

// ClaimPendingBroadcastRecipients atomically claims up to limit not-yet-
// processed recipients: the SKIP LOCKED select and the claimed_at stamp
// happen in ONE statement, so the claim survives past the statement's own
// implicit transaction (a plain SELECT ... FOR UPDATE SKIP LOCKED releases
// its row locks as soon as the query completes — before any Go-side
// processing runs — so two truly-concurrent expanders could otherwise both
// claim and materialize, and charge quota for, the same recipient; see PR
// review). claimed_at recency (recipientClaimLease) keeps a claimed row
// unavailable to other claimants for the lease duration even after the SQL
// statement's own lock releases, covering the actual processing window.
func (db *DB) ClaimPendingBroadcastRecipients(ctx context.Context, broadcastID string, limit int) ([]BroadcastRecipient, error) {
	rows, err := db.pool.Query(ctx, `
		UPDATE broadcast_recipients SET claimed_at = now()
		WHERE id IN (
			SELECT id FROM broadcast_recipients
			WHERE broadcast_id = $1 AND status = 'pending'
			  AND (claimed_at IS NULL OR claimed_at < now() - $3::interval)
			ORDER BY created_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		RETURNING `+broadcastRecipientColumns, broadcastID, limit, recipientClaimLease.String())
	if err != nil {
		return nil, fmt.Errorf("database: claim pending broadcast recipients: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []BroadcastRecipient
	for rows.Next() {
		r, err := scanBroadcastRecipient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// maxRecipientAttempts bounds retries of a deterministically-failing
// recipient (e.g. a render/MIME error): after this many failed attempts the
// recipient moves to the terminal 'failed' status instead of being retried
// (and recharged against tenant quota) forever, which would otherwise also
// block its broadcast from ever completing (see PR review).
const maxRecipientAttempts = 5

// RecordBroadcastRecipientFailure increments the recipient's attempt count
// and, once maxRecipientAttempts is reached, moves it to the terminal
// 'failed' status (excluded from HasPendingBroadcastRecipients, so the
// broadcast can still complete). Guarded on status='pending' like every
// other recipient transition, so a stray duplicate call is a safe no-op.
func (db *DB) RecordBroadcastRecipientFailure(ctx context.Context, id string) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE broadcast_recipients SET
			attempts = attempts + 1,
			status = CASE WHEN attempts + 1 >= $2 THEN 'failed' ELSE status END,
			updated_at = now()
		WHERE id = $1 AND status = 'pending'`, id, maxRecipientAttempts)
	if err != nil {
		return fmt.Errorf("database: record broadcast recipient failure: %w", normalizeErr(err))
	}
	return nil
}

// MarkBroadcastRecipientSuppressed is terminal: zero SMTP for this
// recipient, no message row is ever created. Guarded by "AND status =
// 'pending'" so a retried call after a crash is a safe no-op.
func (db *DB) MarkBroadcastRecipientSuppressed(ctx context.Context, id string) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE broadcast_recipients SET status = 'suppressed', updated_at = now()
		WHERE id = $1 AND status = 'pending'`, id)
	if err != nil {
		return fmt.Errorf("database: mark broadcast recipient suppressed: %w", normalizeErr(err))
	}
	return nil
}

// MarkBroadcastRecipientMaterialized records that recipient id's message
// (messageID, which IS id — see broadcast_recipients.id's doc) now exists in
// the normal MailX send pipeline. Guarded the same way as
// MarkBroadcastRecipientSuppressed for crash-safe idempotent retry.
func (db *DB) MarkBroadcastRecipientMaterialized(ctx context.Context, id, messageID string) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE broadcast_recipients SET status = 'materialized', message_id = $2, updated_at = now()
		WHERE id = $1 AND status = 'pending'`, id, messageID)
	if err != nil {
		return fmt.Errorf("database: mark broadcast recipient materialized: %w", normalizeErr(err))
	}
	return nil
}

// HasPendingBroadcastRecipients reports (bounded, LIMIT 1) whether any
// recipient row still needs processing.
func (db *DB) HasPendingBroadcastRecipients(ctx context.Context, broadcastID string) (bool, error) {
	var one int
	err := db.pool.QueryRow(ctx, `SELECT 1 FROM broadcast_recipients WHERE broadcast_id = $1 AND status = 'pending' LIMIT 1`, broadcastID).Scan(&one)
	if err == nil {
		return true, nil
	}
	if errors.Is(normalizeErr(err), ErrNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("database: check pending broadcast recipients: %w", normalizeErr(err))
}

// TryCompleteBroadcast marks the broadcast completed when snapshotting has
// finished AND no recipient remains pending. "Completed" means orchestration
// is done — every recipient is either suppressed or handed to the existing
// delivery pipeline — NEVER that mail reached an inbox (see design doc).
func (db *DB) TryCompleteBroadcast(ctx context.Context, id string) (completed bool, err error) {
	pending, err := db.HasPendingBroadcastRecipients(ctx, id)
	if err != nil {
		return false, err
	}
	if pending {
		return false, nil
	}
	tag, err := db.pool.Exec(ctx, `
		UPDATE broadcasts SET status = 'completed', completed_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'expanding' AND snapshot_complete = true`, id)
	if err != nil {
		return false, fmt.Errorf("database: complete broadcast: %w", normalizeErr(err))
	}
	return tag.RowsAffected() > 0, nil
}
