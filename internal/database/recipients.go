package database

import (
	"context"
	"fmt"
	"time"
)

// RecipientStatus is the terminal-ish per-recipient delivery state, derived
// by the caller from internal/bounce.RecipientStatus at the point a
// message's delivery reaches a final outcome. See internal/bounce's own
// documented honesty constraint: a sibling recipient that was never
// individually confirmed is still "failed", never fabricated "delivered".
type RecipientStatus string

const (
	RecipientPending   RecipientStatus = "pending"
	RecipientDelivered RecipientStatus = "delivered"
	RecipientFailed    RecipientStatus = "failed"
	// RecipientSuppressed means the recipient was skipped by suppression policy:
	// no SMTP attempt named it. Not failed, not bounced.
	RecipientSuppressed RecipientStatus = "suppressed"
)

func (s RecipientStatus) valid() bool {
	switch s {
	case RecipientPending, RecipientDelivered, RecipientFailed, RecipientSuppressed:
		return true
	}
	return false
}

// Recipient is one persisted envelope (RCPT TO) recipient.
type Recipient struct {
	ID             string
	MessageID      string
	Address        string
	HeaderKind     *string
	Status         RecipientStatus
	SMTPCode       *int
	EnhancedStatus *string
	Diagnostic     *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ListRecipients returns every recipient for a message, insertion order
// (id is not ordering-meaningful, so this orders by created_at then id for
// determinism).
func (db *DB) ListRecipients(ctx context.Context, messageID string) ([]Recipient, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, message_id, address, header_kind, status, smtp_code, enhanced_status, diagnostic, created_at, updated_at
		FROM recipients WHERE message_id = $1 ORDER BY created_at, id`,
		messageID,
	)
	if err != nil {
		return nil, fmt.Errorf("database: list recipients: %w", normalizeErr(err))
	}
	defer rows.Close()

	var out []Recipient
	for rows.Next() {
		var r Recipient
		if err := rows.Scan(&r.ID, &r.MessageID, &r.Address, &r.HeaderKind, &r.Status,
			&r.SMTPCode, &r.EnhancedStatus, &r.Diagnostic, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("database: scan recipient: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecipientStatusUpdate is one recipient's terminal status, as produced by
// internal/bounce.RecipientStatuses. address (not id) identifies the
// recipient, since that is what the delivery/bounce layers actually know.
type RecipientStatusUpdate struct {
	Address        string
	Status         RecipientStatus
	SMTPCode       int
	EnhancedStatus string
	Diagnostic     string
}

// UpdateRecipientStatuses applies terminal recipient statuses for one
// message as a single transaction — a partial update (some recipients
// updated, others not) is never observable for one delivery outcome.
func (db *DB) UpdateRecipientStatuses(ctx context.Context, messageID string, updates []RecipientStatusUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin update recipient statuses: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, u := range updates {
		if !u.Status.valid() {
			return fmt.Errorf("database: invalid recipient status %q", u.Status)
		}
		var code *int
		if u.SMTPCode != 0 {
			code = &u.SMTPCode
		}
		var enhanced, diag *string
		if u.EnhancedStatus != "" {
			enhanced = &u.EnhancedStatus
		}
		if u.Diagnostic != "" {
			diag = &u.Diagnostic
		}
		tag, err := tx.Exec(ctx, `
			UPDATE recipients
			SET status = $1, smtp_code = $2, enhanced_status = $3, diagnostic = $4, updated_at = now()
			WHERE message_id = $5 AND address = $6`,
			u.Status, code, enhanced, diag, messageID, u.Address,
		)
		if err != nil {
			return fmt.Errorf("database: update recipient %q: %w", u.Address, normalizeErr(err))
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: recipient %q on message %q", ErrNotFound, u.Address, messageID)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit update recipient statuses: %w", normalizeErr(err))
	}
	return nil
}
