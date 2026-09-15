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
	StatusDelivered  MessageStatus = "delivered"
	StatusFailed     MessageStatus = "failed"
	StatusBounced    MessageStatus = "bounced"
)

func (s MessageStatus) valid() bool {
	switch s {
	case StatusQueued, StatusProcessing, StatusDelivered, StatusFailed, StatusBounced:
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

	var msg Message
	err = tx.QueryRow(ctx, `
		INSERT INTO messages (id, tenant_id, mail_from, from_header, subject, message_id_header, status, queued_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'queued', now())
		RETURNING id, tenant_id, mail_from, from_header, subject, message_id_header, status, created_at, updated_at, queued_at, delivered_at`,
		in.ID, in.TenantID, in.MailFrom, in.FromHeader, in.Subject, in.MessageIDHeader,
	).Scan(&msg.ID, &msg.TenantID, &msg.MailFrom, &msg.FromHeader, &msg.Subject, &msg.MessageIDHeader,
		&msg.Status, &msg.CreatedAt, &msg.UpdatedAt, &msg.QueuedAt, &msg.DeliveredAt)
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
