package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MessageStatus is the authoritative message-level lifecycle state,
// derived by the caller from delivery/retry outcomes.
type MessageStatus string

const (
	StatusQueued     MessageStatus = "queued"
	StatusProcessing MessageStatus = "processing"
	StatusRetrying   MessageStatus = "retrying"
	StatusDelivered  MessageStatus = "delivered"
	StatusFailed     MessageStatus = "failed"
	StatusBounced    MessageStatus = "bounced"
	// StatusSuppressed is terminal: every recipient was suppressed, so no SMTP
	// attempt was made. It is not a failure and not a bounce.
	StatusSuppressed MessageStatus = "suppressed"
)

// Valid reports whether s is one of the known message statuses, for
// callers (e.g. internal/api) validating a status filter before it
// reaches a query.
func (s MessageStatus) Valid() bool { return s.valid() }

func (s MessageStatus) valid() bool {
	switch s {
	case StatusQueued, StatusProcessing, StatusRetrying, StatusDelivered, StatusFailed, StatusBounced, StatusSuppressed:
		return true
	}
	return false
}

// Message is the durable record of one accepted message; raw MIME lives
// in internal/storage under the same id.
type Message struct {
	ID              string
	TenantID        string
	MailFrom        string
	FromHeader      string
	Subject         string
	MessageIDHeader string
	Status          MessageStatus
	CreatedAt       time.Time
	UpdatedAt       time.Time
	QueuedAt        *time.Time
	DeliveredAt     *time.Time
}

// RecipientInput is one envelope recipient; HeaderKind is nil for an
// envelope-only (e.g. hidden Bcc) recipient.
type RecipientInput struct {
	Address    string
	HeaderKind *string
}

// NewMessage is InsertMessage's input. ID must match the id
// internal/storage already generated for this message's raw content.
type NewMessage struct {
	ID              string
	TenantID        string
	MailFrom        string
	FromHeader      string
	Subject         string
	MessageIDHeader string
	Recipients      []RecipientInput
	// AvailableAt is when the outbox row becomes dispatchable; zero means
	// now (immediate send). A future time defers dispatch (scheduled send).
	AvailableAt time.Time
	// IdempotencyCompletion, if set, marks the ClaimIdempotencyKey row
	// this message fulfills as completed IN THE SAME TRANSACTION as the
	// message/recipients/event/outbox insert below — so "message
	// committed but idempotency record missing" and "idempotency says
	// complete but message failed" cannot happen; there is only one
	// commit for both.
	IdempotencyCompletion *IdempotencyCompletion
	// SenderDomain, when non-empty, is the canonical From domain. InsertMessage
	// re-checks in the same transaction that the tenant still has this domain
	// verified and not deleted (FOR SHARE), so a domain deleted between the API's
	// pre-check and the commit cannot slip an unauthorized message through.
	SenderDomain string
	// RequireBroadcastRecipientID, when non-empty, re-checks in the SAME
	// transaction (FOR UPDATE - a real row lock, not just a read) that this
	// broadcast_recipients row still exists before inserting the message.
	// The broadcast expander sets this: it claims a recipient's snapshot
	// (email/name/attributes) into memory well before this insert, and a
	// GDPR erasure (database.DeleteSubjectData) deleting that same row
	// concurrently must not still let this message go out to the erased
	// address. FOR UPDATE means whichever transaction (this insert, or the
	// erasure's DELETE) commits first wins; the other sees a consistent
	// state instead of racing on stale in-memory data. See ErrRecipientErased.
	RequireBroadcastRecipientID string
}

// ErrSenderNotAuthorized means the tenant does not (or no longer does) own the
// From domain as a verified domain.
var ErrSenderNotAuthorized = errors.New("database: sender domain is not authorized for this tenant")

// ErrRecipientErased means RequireBroadcastRecipientID no longer exists -
// the recipient was removed (e.g. by a GDPR erasure) after being claimed
// for materialization. Callers should treat this as a successful skip
// (the recipient's own request to be forgotten wins), never as a failure
// to retry.
var ErrRecipientErased = errors.New("database: broadcast recipient no longer exists")

// IdempotencyCompletion identifies the idempotency_keys row to complete.
// TenantID is taken from NewMessage.TenantID (the two must always agree,
// so it is not repeated here). Fingerprint MUST be the exact value this
// caller claimed the row with (see database.ClaimIdempotencyKey) - it is
// re-checked at completion time, not just at claim time, because the row
// can be reclaimed by a DIFFERENT caller (with a different fingerprint)
// in between if this caller stalls past the staleness window. Without
// this check a stalled claimant could still complete using the
// reclaimer's now-current row, marking it 'completed' with the WRONG
// caller's resource_id under the reclaimer's fingerprint - a future
// replay for the reclaimer's payload would then return the stale
// claimant's unrelated message.
type IdempotencyCompletion struct {
	Operation      string
	IdempotencyKey string
	Fingerprint    string
}

func (n NewMessage) validate() error {
	if n.ID == "" {
		return errors.New("database: message ID is empty")
	}
	if n.TenantID == "" {
		return errors.New("database: tenant ID is empty")
	}
	if n.MailFrom == "" {
		return errors.New("database: mail_from is empty")
	}
	if len(n.Recipients) == 0 {
		return errors.New("database: message has no recipients")
	}
	for _, r := range n.Recipients {
		if r.Address == "" {
			return errors.New("database: recipient address is empty")
		}
		if r.HeaderKind != nil {
			switch *r.HeaderKind {
			case "to", "cc", "bcc":
			default:
				return fmt.Errorf("database: invalid recipient header_kind %q", *r.HeaderKind)
			}
		}
	}
	return nil
}

// InsertMessage creates the message row, its recipient rows, and an
// initial "queued" event as ONE atomic transaction — a message with no
// recipients, or recipients with no owning message, is never observable.
// Recipients are inserted in a single batched statement rather than one
// round trip per recipient.
func (db *DB) InsertMessage(ctx context.Context, in NewMessage) (Message, error) {
	if err := in.validate(); err != nil {
		return Message{}, err
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return Message{}, fmt.Errorf("database: begin insert message: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if in.RequireBroadcastRecipientID != "" {
		var exists int
		err := tx.QueryRow(ctx, `SELECT 1 FROM broadcast_recipients WHERE id = $1 FOR UPDATE`,
			in.RequireBroadcastRecipientID).Scan(&exists)
		if errors.Is(normalizeErr(err), ErrNotFound) {
			return Message{}, ErrRecipientErased
		}
		if err != nil {
			return Message{}, fmt.Errorf("database: check broadcast recipient still exists: %w", normalizeErr(err))
		}
	}

	// sendingPoolID is the SenderDomain's operator-assigned pool, read under
	// the SAME FOR SHARE lock as the verified-domain check below, so it
	// reflects a domain state that cannot change out from under this
	// transaction. Route selection (v0.39) piggybacks on this existing
	// domain check rather than adding a second query, and runs in the same
	// transaction as the message insert so the durable routing decision
	// (messages.sending_member_id) and the message become visible together.
	var sendingPoolID *string
	if in.SenderDomain != "" {
		err := tx.QueryRow(ctx, `SELECT sending_pool_id FROM domains
			WHERE tenant_id = $1 AND name = $2 AND deleted_at IS NULL AND verification_status = 'verified' FOR SHARE`,
			in.TenantID, in.SenderDomain).Scan(&sendingPoolID)
		if errors.Is(normalizeErr(err), ErrNotFound) {
			return Message{}, ErrSenderNotAuthorized
		}
		if err != nil {
			return Message{}, fmt.Errorf("database: check sender domain: %w", normalizeErr(err))
		}
	}
	sendingMemberID, err := selectRoutingMember(ctx, tx, in.ID, sendingPoolID)
	if err != nil {
		return Message{}, err
	}
	var sendingMemberArg *string
	if sendingMemberID != "" {
		sendingMemberArg = &sendingMemberID
	}

	var msg Message
	if sendingMemberArg != nil {
		// Only touch sending_member_id (migration 000023) when a route was
		// actually selected. This keeps the common/legacy no-pool insert
		// identical to the pre-v0.39 statement — load-bearing for this
		// package's migration-upgrade regression tests (see
		// TestWebhookMigrationUpgradesDurabilityPrerequisite and
		// TestDeliveryOutcomeMigrationUpgradesV021Data), which roll a test
		// schema back to the previous migration(s) and insert a message
		// with CURRENT Go code before upgrading — exercising exactly the
		// rolling-deploy window where new app code briefly runs against an
		// older schema. A message can only ever get a non-nil member
		// (below) via SenderDomain's pool assignment, which itself
		// requires migration 000023's domains.sending_pool_id — so this
		// branch is only reachable once that migration has actually run.
		err = tx.QueryRow(ctx, `
			INSERT INTO messages (id, tenant_id, mail_from, from_header, subject, message_id_header, status, queued_at, sending_member_id)
			VALUES ($1, $2, $3, $4, $5, $6, 'queued', now(), $7)
			RETURNING id, tenant_id, mail_from, from_header, subject, message_id_header, status, created_at, updated_at, queued_at, delivered_at`,
			in.ID, in.TenantID, in.MailFrom, in.FromHeader, in.Subject, in.MessageIDHeader, sendingMemberArg,
		).Scan(&msg.ID, &msg.TenantID, &msg.MailFrom, &msg.FromHeader, &msg.Subject, &msg.MessageIDHeader,
			&msg.Status, &msg.CreatedAt, &msg.UpdatedAt, &msg.QueuedAt, &msg.DeliveredAt)
	} else {
		err = tx.QueryRow(ctx, `
			INSERT INTO messages (id, tenant_id, mail_from, from_header, subject, message_id_header, status, queued_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'queued', now())
			RETURNING id, tenant_id, mail_from, from_header, subject, message_id_header, status, created_at, updated_at, queued_at, delivered_at`,
			in.ID, in.TenantID, in.MailFrom, in.FromHeader, in.Subject, in.MessageIDHeader,
		).Scan(&msg.ID, &msg.TenantID, &msg.MailFrom, &msg.FromHeader, &msg.Subject, &msg.MessageIDHeader,
			&msg.Status, &msg.CreatedAt, &msg.UpdatedAt, &msg.QueuedAt, &msg.DeliveredAt)
	}
	if err != nil {
		return Message{}, fmt.Errorf("database: insert message: %w", normalizeErr(err))
	}

	batch := &pgx.Batch{}
	for _, r := range in.Recipients {
		rid, idErr := newID()
		if idErr != nil {
			return Message{}, idErr
		}
		batch.Queue(`INSERT INTO recipients (id, message_id, address, header_kind) VALUES ($1, $2, $3, $4)`,
			rid, msg.ID, r.Address, r.HeaderKind)
	}
	results := tx.SendBatch(ctx, batch)
	for range in.Recipients {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return Message{}, fmt.Errorf("database: insert recipient: %w", normalizeErr(err))
		}
	}
	if err := results.Close(); err != nil {
		return Message{}, fmt.Errorf("database: close recipient batch: %w", normalizeErr(err))
	}

	eventID, err := newID()
	if err != nil {
		return Message{}, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO events (id, tenant_id, message_id, event_type) VALUES ($1, $2, $3, 'queued')`,
		eventID, msg.TenantID, msg.ID,
	); err != nil {
		return Message{}, fmt.Errorf("database: insert queued event: %w", normalizeErr(err))
	}

	availableAt := in.AvailableAt
	if availableAt.IsZero() {
		availableAt = msg.CreatedAt
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox (message_id, tenant_id, available_at) VALUES ($1, $2, $3)`,
		msg.ID, msg.TenantID, availableAt,
	); err != nil {
		return Message{}, fmt.Errorf("database: insert outbox row: %w", normalizeErr(err))
	}

	if in.IdempotencyCompletion != nil {
		ic := in.IdempotencyCompletion
		tag, err := tx.Exec(ctx, `
			UPDATE idempotency_keys SET status = 'completed', resource_id = $1, completed_at = now()
			WHERE tenant_id = $2 AND operation = $3 AND idempotency_key = $4
			  AND status = 'in_progress' AND fingerprint = $5`,
			msg.ID, msg.TenantID, ic.Operation, ic.IdempotencyKey, ic.Fingerprint,
		)
		if err != nil {
			return Message{}, fmt.Errorf("database: complete idempotency key: %w", normalizeErr(err))
		}
		if tag.RowsAffected() == 0 {
			// The claim this message was meant to fulfill is gone (already
			// completed by someone else, or reclaimed out from under us
			// after a long stall) - committing anyway would durably create
			// a message no longer backed by a valid claim. Roll back
			// instead; the caller lost the ownership race.
			return Message{}, fmt.Errorf("database: idempotency claim %s/%s no longer owned: %w", ic.Operation, ic.IdempotencyKey, ErrConflict)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Message{}, fmt.Errorf("database: commit insert message: %w", normalizeErr(err))
	}
	return msg, nil
}

// GetMessage loads one message, scoped to tenantID so a caller cannot
// accidentally fetch another tenant's message by guessing/reusing an ID.
func (db *DB) GetMessage(ctx context.Context, tenantID, id string) (Message, error) {
	var msg Message
	err := db.pool.QueryRow(ctx, `
		SELECT id, tenant_id, mail_from, from_header, subject, message_id_header, status, created_at, updated_at, queued_at, delivered_at
		FROM messages WHERE tenant_id = $1 AND id = $2`,
		tenantID, id,
	).Scan(&msg.ID, &msg.TenantID, &msg.MailFrom, &msg.FromHeader, &msg.Subject, &msg.MessageIDHeader,
		&msg.Status, &msg.CreatedAt, &msg.UpdatedAt, &msg.QueuedAt, &msg.DeliveredAt)
	if err != nil {
		return Message{}, normalizeErr(err)
	}
	return msg, nil
}

// MessageCursor is an opaque keyset-pagination position: the (created_at,
// id) of the last row on the previous page. Using created_at+id (rather
// than OFFSET) keeps listing efficient regardless of how deep the caller
// pages — see the v0.15 report's pagination discussion.
type MessageCursor struct {
	CreatedAt time.Time
	ID        string
}

// ListMessages returns up to limit messages for tenantID, optionally
// filtered by status, newest first. Pass a non-nil after cursor (from a
// previous page's last row) to continue listing. The query and its
// ordering match idx_messages_tenant_status_created exactly.
func (db *DB) ListMessages(ctx context.Context, tenantID string, status *MessageStatus, limit int, after *MessageCursor) ([]Message, error) {
	if tenantID == "" {
		return nil, errors.New("database: tenant ID is empty")
	}
	if limit <= 0 || limit > 500 {
		return nil, fmt.Errorf("database: limit must be between 1 and 500, got %d", limit)
	}
	if status != nil && !status.valid() {
		return nil, fmt.Errorf("database: invalid status %q", *status)
	}

	query := `
		SELECT id, tenant_id, mail_from, from_header, subject, message_id_header, status, created_at, updated_at, queued_at, delivered_at
		FROM messages
		WHERE tenant_id = $1
		  AND ($2::text IS NULL OR status = $2)
		  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4))
		ORDER BY created_at DESC, id DESC
		LIMIT $5`

	var statusArg *string
	if status != nil {
		s := string(*status)
		statusArg = &s
	}
	var afterTime *time.Time
	var afterID *string
	if after != nil {
		afterTime = &after.CreatedAt
		afterID = &after.ID
	}

	rows, err := db.pool.Query(ctx, query, tenantID, statusArg, afterTime, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("database: list messages: %w", normalizeErr(err))
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var msg Message
		if err := rows.Scan(&msg.ID, &msg.TenantID, &msg.MailFrom, &msg.FromHeader, &msg.Subject, &msg.MessageIDHeader,
			&msg.Status, &msg.CreatedAt, &msg.UpdatedAt, &msg.QueuedAt, &msg.DeliveredAt); err != nil {
			return nil, fmt.Errorf("database: scan message: %w", err)
		}
		out = append(out, msg)
	}
	return out, rows.Err()
}

// UpdateMessageStatus transitions a message's authoritative status.
// deliveredAt is set only when transitioning to StatusDelivered (callers
// pass nil otherwise); updated_at always advances.
func (db *DB) UpdateMessageStatus(ctx context.Context, id string, status MessageStatus, deliveredAt *time.Time) error {
	if !status.valid() {
		return fmt.Errorf("database: invalid status %q", status)
	}
	tag, err := db.pool.Exec(ctx,
		`UPDATE messages SET status = $1, updated_at = now(), delivered_at = COALESCE($2, delivered_at) WHERE id = $3`,
		status, deliveredAt, id,
	)
	if err != nil {
		return fmt.Errorf("database: update message status: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
