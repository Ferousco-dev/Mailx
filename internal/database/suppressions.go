package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Ferousco-dev/mailx/internal/suppression"
)

// Suppression is one durable "do not send to this address" policy entry.
type Suppression struct {
	ID             string
	TenantID       string
	Email          string // canonical key (suppression.Normalize)
	Reason         suppression.Reason
	Source         suppression.Source
	MessageID      *string // evidence for a hard bounce
	SMTPCode       *int
	EnhancedStatus *string
	CreatedAt      time.Time
}

// NewSuppression describes an entry to create. Email may be any form
// suppression.Normalize accepts; the store always keys by the canonical form, so
// no caller can create a key another caller's lookup would not find.
type NewSuppression struct {
	TenantID       string
	Email          string
	Reason         suppression.Reason
	Source         suppression.Source
	MessageID      string
	SMTPCode       int
	EnhancedStatus string
}

// SuppressionCursor is the keyset position of a list page.
type SuppressionCursor struct {
	CreatedAt time.Time
	ID        string
}

// ErrInvalidSuppression is returned for a malformed entry (never for a database failure).
var ErrInvalidSuppression = errors.New("database: invalid suppression")

const suppressionColumns = `id, tenant_id, email, reason, source, message_id, smtp_code, enhanced_status, created_at`

func scanSuppression(row rowScanner) (Suppression, error) {
	var s Suppression
	var reason, source string
	err := row.Scan(&s.ID, &s.TenantID, &s.Email, &reason, &source, &s.MessageID, &s.SMTPCode, &s.EnhancedStatus, &s.CreatedAt)
	s.Reason, s.Source = suppression.Reason(reason), suppression.Source(source)
	return s, normalizeErr(err)
}

// CreateSuppression inserts the entry, or returns the existing one for the same
// tenant and address. created reports whether THIS call inserted the row. It is
// safe under any concurrency: the (tenant_id, email) UNIQUE constraint decides, so
// many API requests and workers creating the same suppression produce exactly one
// row, and the first writer's reason/source are kept (a later duplicate never
// overwrites them).
func (db *DB) CreateSuppression(ctx context.Context, in NewSuppression) (s Suppression, created bool, err error) {
	email, err := suppression.Normalize(in.Email)
	if err != nil || in.TenantID == "" || !in.Reason.Valid() || !in.Source.Valid() {
		return Suppression{}, false, ErrInvalidSuppression
	}
	id, err := newID()
	if err != nil {
		return Suppression{}, false, err
	}
	s, err = scanSuppression(db.pool.QueryRow(ctx, `
		INSERT INTO suppressions (id, tenant_id, email, reason, source, message_id, smtp_code, enhanced_status)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, 0), NULLIF($8, ''))
		ON CONFLICT (tenant_id, email) DO NOTHING
		RETURNING `+suppressionColumns,
		id, in.TenantID, email, string(in.Reason), string(in.Source), in.MessageID, in.SMTPCode, in.EnhancedStatus))
	if err == nil {
		return s, true, nil
	}
	if !errors.Is(err, ErrNotFound) { // DO NOTHING returns no row
		return Suppression{}, false, fmt.Errorf("database: create suppression: %w", err)
	}
	existing, err := scanSuppression(db.pool.QueryRow(ctx, `SELECT `+suppressionColumns+`
		FROM suppressions WHERE tenant_id = $1 AND email = $2`, in.TenantID, email))
	if err != nil { // deleted between the two statements: report it, callers may retry
		return Suppression{}, false, fmt.Errorf("database: read existing suppression: %w", err)
	}
	return existing, false, nil
}

// GetSuppression returns one entry of the tenant (ErrNotFound for another
// tenant's id, indistinguishable from a missing one).
func (db *DB) GetSuppression(ctx context.Context, tenantID, id string) (Suppression, error) {
	return scanSuppression(db.pool.QueryRow(ctx, `SELECT `+suppressionColumns+`
		FROM suppressions WHERE tenant_id = $1 AND id = $2`, tenantID, id))
}

// DeleteSuppression removes an entry (hard delete). It changes NO delivery
// history: terminal messages stay terminal and nothing is re-queued.
func (db *DB) DeleteSuppression(ctx context.Context, tenantID, id string) error {
	tag, err := db.pool.Exec(ctx, `DELETE FROM suppressions WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("database: delete suppression: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListSuppressions pages a tenant's entries newest first by keyset (created_at,
// id). A non-empty email filters to that canonical address (at most one row).
func (db *DB) ListSuppressions(ctx context.Context, tenantID string, limit int, after *SuppressionCursor, email string) ([]Suppression, error) {
	if tenantID == "" || limit < 1 || limit > 500 {
		return nil, errors.New("database: invalid suppression list arguments")
	}
	var afterTime *time.Time
	var afterID *string
	if after != nil {
		afterTime, afterID = &after.CreatedAt, &after.ID
	}
	var key *string
	if email != "" {
		k, err := suppression.Normalize(email)
		if err != nil {
			return nil, ErrInvalidSuppression
		}
		key = &k
	}
	rows, err := db.pool.Query(ctx, `SELECT `+suppressionColumns+`
		FROM suppressions
		WHERE tenant_id = $1
		  AND ($2::text IS NULL OR email = $2)
		  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4))
		ORDER BY created_at DESC, id DESC
		LIMIT $5`, tenantID, key, afterTime, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("database: list suppressions: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []Suppression
	for rows.Next() {
		s, err := scanSuppression(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SuppressedForTenant returns the canonical keys among emails that are suppressed
// for the tenant: ONE indexed query however many recipients there are.
// Callers pass canonical keys (suppression.Normalize).
func (db *DB) SuppressedForTenant(ctx context.Context, tenantID string, emails []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(emails) == 0 {
		return out, nil
	}
	rows, err := db.pool.Query(ctx, `SELECT email FROM suppressions WHERE tenant_id = $1 AND email = ANY($2)`, tenantID, emails)
	if err != nil {
		return nil, fmt.Errorf("database: check suppressions: %w", normalizeErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, fmt.Errorf("database: scan suppression: %w", err)
		}
		out[e] = true
	}
	return out, rows.Err()
}

// SuppressedForMessage is SuppressedForTenant for the tenant that OWNS the message,
// resolved in the same query, so a worker needs only the message id.
func (db *DB) SuppressedForMessage(ctx context.Context, messageID string, emails []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(emails) == 0 {
		return out, nil
	}
	rows, err := db.pool.Query(ctx, `
		SELECT s.email FROM messages m
		JOIN suppressions s ON s.tenant_id = m.tenant_id
		WHERE m.id = $1 AND s.email = ANY($2)`, messageID, emails)
	if err != nil {
		return nil, fmt.Errorf("database: check suppressions for message: %w", normalizeErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, fmt.Errorf("database: scan suppression: %w", err)
		}
		out[e] = true
	}
	return out, rows.Err()
}

// RecordSuppressedRecipients durably records that recipients of a message were
// skipped because they are suppressed: their rows become 'suppressed' and ONE
// 'suppressed' lifecycle event is appended per message (at most; replays are no-ops).
// When allSuppressed is true nothing can be delivered, and the message becomes the
// terminal 'suppressed' state in the SAME transaction, so a crash can never leave a
// terminal-looking event without the terminal status or vice versa.
//
// It never takes a lock across network I/O (the row lock lives only for this
// short transaction) and never touches a message that already reached a terminal
// status other than 'suppressed' (ErrMessageTerminal): accepted delivery history is
// immutable. Idempotent.
func (db *DB) RecordSuppressedRecipients(ctx context.Context, messageID string, suppressedKeys map[string]bool, allSuppressed bool) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin record suppressed: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var tenantID string
	var current MessageStatus
	if err := tx.QueryRow(ctx, `SELECT tenant_id, status FROM messages WHERE id = $1 FOR UPDATE`, messageID).Scan(&tenantID, &current); err != nil {
		return fmt.Errorf("database: lock message for suppression: %w", normalizeErr(err))
	}
	if current == StatusSuppressed {
		return nil // idempotent replay
	}
	if terminalMessageStatus(current) {
		return ErrMessageTerminal
	}

	rows, err := tx.Query(ctx, `SELECT id, address FROM recipients WHERE message_id = $1 AND status = 'pending'`, messageID)
	if err != nil {
		return fmt.Errorf("database: read recipients: %w", normalizeErr(err))
	}
	var ids []string
	for rows.Next() {
		var id, addr string
		if err := rows.Scan(&id, &addr); err != nil {
			rows.Close()
			return fmt.Errorf("database: scan recipient: %w", err)
		}
		if key, err := suppression.Normalize(addr); err == nil && suppressedKeys[key] {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("database: read recipients: %w", normalizeErr(err))
	}
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE recipients SET status = 'suppressed', updated_at = now() WHERE id = ANY($1)`, ids); err != nil {
			return fmt.Errorf("database: mark recipients suppressed: %w", normalizeErr(err))
		}
	}
	eventID, err := newID()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO events (id, tenant_id, message_id, event_type, metadata)
		VALUES ($1, $2, $3, 'suppressed', jsonb_build_object('suppressed_recipients', $4::int, 'all_recipients', $5::boolean))
		ON CONFLICT (message_id) WHERE event_type = 'suppressed' DO NOTHING`,
		eventID, tenantID, messageID, len(ids), allSuppressed); err != nil {
		return fmt.Errorf("database: append suppressed event: %w", normalizeErr(err))
	}
	if allSuppressed {
		if _, err := tx.Exec(ctx, `UPDATE messages SET status = 'suppressed', updated_at = now() WHERE id = $1`, messageID); err != nil {
			return fmt.Errorf("database: mark message suppressed: %w", normalizeErr(err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit record suppressed: %w", normalizeErr(err))
	}
	return nil
}
