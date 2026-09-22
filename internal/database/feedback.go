package database

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/Ferousco-dev/mailx/internal/feedback"
	"github.com/Ferousco-dev/mailx/internal/suppression"
)

// ErrRecipientNotFound means the feedback's recipient did not match any
// recipient row of the correlated message.
var ErrRecipientNotFound = errors.New("database: feedback recipient not found on message")

// ProcessFeedback durably records one Feedback against messageID, in one
// transaction: feedback row (idempotent, dedup by raw hash), recipient
// feedback_status transition (only if new), suppression (only on a real
// transition), and event (only on a real transition). Never depends on
// webhook success. tenant is read from the message row, never from the
// caller.
func (db *DB) ProcessFeedback(ctx context.Context, messageID string, f feedback.Feedback) (created bool, err error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("database: begin feedback: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var tenantID string
	if err := tx.QueryRow(ctx, `SELECT tenant_id FROM messages WHERE id = $1`, messageID).Scan(&tenantID); err != nil {
		if errors.Is(normalizeErr(err), ErrNotFound) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("database: lookup message tenant: %w", normalizeErr(err))
	}

	key, err := suppression.Normalize(f.FinalRecipient)
	if err != nil {
		return false, ErrRecipientNotFound
	}
	var recipientID, address string
	rows, err := tx.Query(ctx, `SELECT id, address FROM recipients WHERE message_id = $1`, messageID)
	if err != nil {
		return false, fmt.Errorf("database: read recipients: %w", normalizeErr(err))
	}
	for rows.Next() {
		var id, addr string
		if err := rows.Scan(&id, &addr); err != nil {
			rows.Close()
			return false, fmt.Errorf("database: scan recipient: %w", err)
		}
		if k, err := suppression.Normalize(addr); err == nil && k == key {
			recipientID, address = id, addr
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if recipientID == "" {
		return false, ErrRecipientNotFound
	}

	id, err := newID()
	if err != nil {
		return false, err
	}
	var action *string
	if f.Action != "" {
		s := string(f.Action)
		action = &s
	}
	var enhanced, diag, remoteMTA, reportingMTA *string
	if f.EnhancedStatus != "" {
		enhanced = &f.EnhancedStatus
	}
	if f.Diagnostic != "" {
		diag = &f.Diagnostic
	}
	if f.RemoteMTA != "" {
		remoteMTA = &f.RemoteMTA
	}
	if f.ReportingMTA != "" {
		reportingMTA = &f.ReportingMTA
	}
	rawHash := hex.EncodeToString(f.RawSHA256[:])

	tag, err := tx.Exec(ctx, `
		INSERT INTO feedback (id, tenant_id, message_id, recipient_id, kind, source, action, enhanced_status, diagnostic, remote_mta, reporting_mta, raw_sha256, received_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (message_id, recipient_id, raw_sha256) DO NOTHING`,
		id, tenantID, messageID, recipientID, string(f.Kind), string(f.Source), action, enhanced, diag, remoteMTA, reportingMTA, rawHash, f.ReceivedAt)
	if err != nil {
		return false, fmt.Errorf("database: insert feedback: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return false, tx.Commit(ctx) // exact duplicate: no-op, already committed as a successful no-op
	}

	// Only bounce_permanent and complaint carry a terminal recipient
	// feedback_status; temporary/unknown are recorded as history only (see
	// feedback.Classify's doc).
	var newStatus string
	switch f.Kind {
	case feedback.KindBouncePermanent:
		newStatus = "bounced"
	case feedback.KindComplaint:
		newStatus = "complained"
	default:
		return true, tx.Commit(ctx)
	}

	var transitioned bool
	err = tx.QueryRow(ctx, `
		UPDATE recipients SET feedback_status = $2, feedback_at = $3, feedback_enhanced_status = $4
		WHERE id = $1 AND feedback_status IS DISTINCT FROM $2
		RETURNING true`, recipientID, newStatus, f.ReceivedAt, enhanced).Scan(&transitioned)
	if err != nil && !errors.Is(normalizeErr(err), ErrNotFound) {
		return false, fmt.Errorf("database: update recipient feedback: %w", normalizeErr(err))
	}
	if !transitioned {
		return true, tx.Commit(ctx) // already at this feedback state; no suppression/event re-fire
	}

	shouldSuppress := f.Kind == feedback.KindComplaint ||
		(f.Kind == feedback.KindBouncePermanent && f.EnhancedStatus != "" && suppression.QualifiesAsyncHardBounce(f.EnhancedStatus))
	if shouldSuppress {
		reason := suppression.ReasonHardBounce
		if f.Kind == feedback.KindComplaint {
			reason = suppression.ReasonComplaint
		}
		key, _ := suppression.Normalize(address)
		suppID, err := newID()
		if err != nil {
			return false, err
		}
		var es string
		if enhanced != nil {
			es = *enhanced
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO suppressions (id, tenant_id, email, reason, source, message_id, enhanced_status)
			VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''))
			ON CONFLICT (tenant_id, email) DO NOTHING`,
			suppID, tenantID, key, string(reason), string(suppression.SourceFeedback), messageID, es); err != nil {
			return false, fmt.Errorf("database: suppress from feedback: %w", normalizeErr(err))
		}
	}

	eventID, err := newID()
	if err != nil {
		return false, err
	}
	eventType := string(EventBounced)
	if f.Kind == feedback.KindComplaint {
		eventType = string(EventComplained)
	}
	var es string
	if enhanced != nil {
		es = *enhanced
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO events (id, tenant_id, message_id, event_type, metadata)
		VALUES ($1,$2,$3,$4, jsonb_build_object('recipient', $5::text, 'enhanced_status', NULLIF($6,'')))`,
		eventID, tenantID, messageID, eventType, address, es); err != nil {
		return false, fmt.Errorf("database: append feedback event: %w", normalizeErr(err))
	}

	return true, tx.Commit(ctx)
}
