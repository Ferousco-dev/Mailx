package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// EventType is one lifecycle transition MailX records for a message.
type EventType string

const (
	EventQueued            EventType = "queued"
	EventDeliveryAttempted EventType = "delivery_attempted"
	EventDelivered         EventType = "delivered"
	EventDeferred          EventType = "deferred"
	EventBounced           EventType = "bounced"
	EventFailed            EventType = "failed"
	EventSuppressed        EventType = "suppressed" // recipients were skipped by suppression policy
	// EventComplained is v0.32: a verified complaint feedback report matched one
	// of this message's recipients. EventBounced is REUSED (not duplicated) for
	// v0.32's asynchronous bounce feedback — it was reserved since v0.18 and
	// never produced by any synchronous path (see database.StatusBounced's
	// doc); one internal event name for "this message bounced" regardless of
	// whether that was learned synchronously or later keeps the public
	// vocabulary from growing needlessly.
	EventComplained EventType = "complained"
	// EventOpened/EventClicked (v0.43) are opt-in, unreliable-by-nature
	// engagement signals — never treated as proof a human read a message.
	EventOpened  EventType = "opened"
	EventClicked EventType = "clicked"
)

func (e EventType) valid() bool {
	switch e {
	case EventQueued, EventDeliveryAttempted, EventDelivered, EventDeferred, EventBounced, EventFailed, EventSuppressed, EventComplained, EventOpened, EventClicked:
		return true
	}
	return false
}

// PublicEventType maps internal lifecycle names to the stable developer API.
// delivery_attempted is intentionally internal: deferred/delivered/failed are
// the externally meaningful outcomes of an operation.
func PublicEventType(e EventType) (string, bool) {
	switch e {
	case EventQueued:
		return "email.queued", true
	case EventDelivered:
		return "email.delivered", true
	case EventDeferred:
		return "email.delivery_delayed", true
	case EventFailed:
		return "email.failed", true
	case EventBounced:
		return "email.bounced", true
	case EventSuppressed:
		return "email.suppressed", true
	case EventComplained:
		return "email.complained", true
	case EventOpened:
		return "email.opened", true
	case EventClicked:
		return "email.clicked", true
	default:
		return "", false
	}
}

// Event is one append-only lifecycle record.
type Event struct {
	ID                    string
	TenantID              string
	MessageID             string
	Type                  EventType
	OccurredAt            time.Time
	Metadata              map[string]any
	DeliveryAttemptNumber *int
}

// AppendEvent inserts one event. Metadata is small, genuinely
// semi-structured context (e.g. {"attempt_number": 2, "code": 451}) —
// JSONB here follows the same rationale as delivery_attempts.mx_attempts.
func (db *DB) AppendEvent(ctx context.Context, tenantID, messageID string, eventType EventType, metadata map[string]any) (Event, error) {
	if tenantID == "" || messageID == "" {
		return Event{}, errors.New("database: tenant ID and message ID are required")
	}
	if !eventType.valid() {
		return Event{}, fmt.Errorf("database: invalid event type %q", eventType)
	}
	id, err := newID()
	if err != nil {
		return Event{}, err
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		return Event{}, fmt.Errorf("database: marshal event metadata: %w", err)
	}

	var e Event
	var metaRaw []byte
	err = db.pool.QueryRow(ctx, `
		INSERT INTO events (id, tenant_id, message_id, event_type, metadata)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id, tenant_id, message_id, event_type, occurred_at, metadata, delivery_attempt_number`,
		id, tenantID, messageID, eventType, metaJSON,
	).Scan(&e.ID, &e.TenantID, &e.MessageID, &e.Type, &e.OccurredAt, &metaRaw, &e.DeliveryAttemptNumber)
	if err != nil {
		return Event{}, fmt.Errorf("database: append event: %w", normalizeErr(err))
	}
	if err := json.Unmarshal(metaRaw, &e.Metadata); err != nil {
		return Event{}, fmt.Errorf("database: unmarshal event metadata: %w", err)
	}
	return e, nil
}

// ListMessageEvents returns every event for one message, chronologically —
// the future GET /emails/{id} timeline view.
func (db *DB) ListMessageEvents(ctx context.Context, messageID string) ([]Event, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, tenant_id, message_id, event_type, occurred_at, metadata, delivery_attempt_number
		FROM events WHERE message_id = $1 ORDER BY occurred_at, id`,
		messageID,
	)
	if err != nil {
		return nil, fmt.Errorf("database: list message events: %w", normalizeErr(err))
	}
	defer rows.Close()
	return scanEvents(rows)
}

// EventCursor is an opaque keyset-pagination position for tenant-wide
// event listing, matching idx_events_tenant_occurred's column order.
type EventCursor struct {
	OccurredAt time.Time
	ID         string
}

// ListTenantEvents returns up to limit events for tenantID, newest first —
// the future GET /events endpoint. Matches idx_events_tenant_occurred
// exactly; never joins messages (tenant_id is denormalized on events for
// this reason).
func (db *DB) ListTenantEvents(ctx context.Context, tenantID string, limit int, after *EventCursor) ([]Event, error) {
	return db.listTenantEvents(ctx, tenantID, limit, after, false)
}

// ListTenantPublicEvents excludes internal bookkeeping events before applying
// pagination, so an internal event cannot create a short or skipped API page.
func (db *DB) ListTenantPublicEvents(ctx context.Context, tenantID string, limit int, after *EventCursor) ([]Event, error) {
	return db.listTenantEvents(ctx, tenantID, limit, after, true)
}

func (db *DB) listTenantEvents(ctx context.Context, tenantID string, limit int, after *EventCursor, publicOnly bool) ([]Event, error) {
	if tenantID == "" {
		return nil, errors.New("database: tenant ID is empty")
	}
	if limit <= 0 || limit > 500 {
		return nil, fmt.Errorf("database: limit must be between 1 and 500, got %d", limit)
	}
	var afterTime *time.Time
	var afterID *string
	if after != nil {
		afterTime = &after.OccurredAt
		afterID = &after.ID
	}
	rows, err := db.pool.Query(ctx, `
		SELECT id, tenant_id, message_id, event_type, occurred_at, metadata, delivery_attempt_number
		FROM events
		WHERE tenant_id = $1
		  AND ($2::timestamptz IS NULL OR (occurred_at, id) < ($2, $3))
		  AND (NOT $5 OR event_type IN ('queued','delivered','deferred','failed','bounced','suppressed','complained','opened','clicked'))
		ORDER BY occurred_at DESC, id DESC
		LIMIT $4`,
		tenantID, afterTime, afterID, limit, publicOnly,
	)
	if err != nil {
		return nil, fmt.Errorf("database: list tenant events: %w", normalizeErr(err))
	}
	defer rows.Close()
	return scanEvents(rows)
}

func scanEvents(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]Event, error) {
	var out []Event
	for rows.Next() {
		var e Event
		var metaRaw []byte
		if err := rows.Scan(&e.ID, &e.TenantID, &e.MessageID, &e.Type, &e.OccurredAt, &metaRaw, &e.DeliveryAttemptNumber); err != nil {
			return nil, fmt.Errorf("database: scan event: %w", err)
		}
		if err := json.Unmarshal(metaRaw, &e.Metadata); err != nil {
			return nil, fmt.Errorf("database: unmarshal event metadata: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
