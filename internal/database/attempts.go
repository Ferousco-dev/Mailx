package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Decision mirrors internal/retry.Decision's three outcomes for one
// retry-level delivery operation. This package intentionally does not
// import internal/retry (database stays infrastructure-level, with no
// dependency on delivery/retry/SMTP concepts) — a caller (the future
// worker/database integration) translates retry.Decision into this type.
type Decision string

const (
	DecisionRetry           Decision = "retry"
	DecisionTerminalSuccess Decision = "terminal_success"
	DecisionTerminalFailure Decision = "terminal_failure"
)

func (d Decision) valid() bool {
	switch d {
	case DecisionRetry, DecisionTerminalSuccess, DecisionTerminalFailure:
		return true
	}
	return false
}

// MXAttempt is the finer MX/SMTP-level truth within one delivery
// operation — mirrors delivery.Attempt. Stored as an element of the
// delivery_attempts.mx_attempts JSONB array; see migrations/000001 for why
// this is JSONB rather than a normalized child table.
type MXAttempt struct {
	Host           string    `json:"host"`
	Preference     int       `json:"preference"`
	Destination    string    `json:"destination"`
	Accepted       bool      `json:"accepted"`
	FailureStage   string    `json:"failure_stage,omitempty"`
	Temporary      bool      `json:"temporary,omitempty"`
	Code           int       `json:"code,omitempty"`
	EnhancedStatus string    `json:"enhanced_status,omitempty"`
	RemoteMessage  string    `json:"remote_message,omitempty"`
	Recipient      string    `json:"recipient,omitempty"`
	AttemptID      string    `json:"attempt_id,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
}

// NewDeliveryAttempt is the input to InsertDeliveryAttempt — one row per
// retry.Coordinator.Attempt call (retry-level operation), matching
// retry.DeliveryAttempt.Number / delivery.Result exactly.
type NewDeliveryAttempt struct {
	MessageID      string
	AttemptNumber  int
	Decision       Decision
	Kind           string
	Accepted       bool
	FinalCode      int
	EnhancedStatus string
	RemoteMessage  string
	FailureStage   string
	Recipient      string
	QuitError      string
	MXAttempts     []MXAttempt
	StartedAt      time.Time
	FinishedAt     time.Time
	// SuppressRecipient asks PersistDeliveryOutcome to also suppress Recipient in
	// the SAME transaction (a qualifying recipient hard bounce; the policy lives in
	// internal/suppression). It is ignored unless the decision is terminal failure.
	SuppressRecipient bool
	// SendingMemberID, TransportKind, EffectiveHostname and
	// EffectiveSourceIP (v0.39) are a historical snapshot of the outbound
	// infrastructure THIS attempt actually used, taken at attempt time.
	// They are deliberately denormalized (no FK to sending_pool_members):
	// unlike messages.sending_member_id (the durable routing decision,
	// which can be looked up live), these must keep answering "what
	// actually sent this" even after the member/pool config later
	// changes. All empty/zero on the legacy no-pool path. Never a
	// secret/credential.
	SendingMemberID   string
	TransportKind     string
	EffectiveHostname string
	EffectiveSourceIP string
}

func (n NewDeliveryAttempt) validate() error {
	if n.MessageID == "" {
		return errors.New("database: delivery attempt message ID is empty")
	}
	if n.AttemptNumber <= 0 {
		return errors.New("database: delivery attempt number must be positive")
	}
	if !n.Decision.valid() {
		return fmt.Errorf("database: invalid delivery attempt decision %q", n.Decision)
	}
	if n.Kind == "" {
		return errors.New("database: delivery attempt kind is empty")
	}
	if n.FinishedAt.Before(n.StartedAt) {
		return errors.New("database: delivery attempt finished before it started")
	}
	return nil
}

// DeliveryAttempt is one persisted retry-level operation.
type DeliveryAttempt struct {
	ID             string
	MessageID      string
	AttemptNumber  int
	Decision       Decision
	Kind           string
	Accepted       bool
	FinalCode      *int
	EnhancedStatus *string
	RemoteMessage  *string
	FailureStage   *string
	Recipient      *string
	QuitError      *string
	MXAttempts     []MXAttempt
	StartedAt      time.Time
	FinishedAt     time.Time
	CreatedAt      time.Time
	// SendingMemberID, TransportKind, EffectiveHostname and
	// EffectiveSourceIP are the historical outbound-infrastructure
	// snapshot described on NewDeliveryAttempt. Nil on the legacy
	// no-pool path.
	SendingMemberID   *string
	TransportKind     *string
	EffectiveHostname *string
	EffectiveSourceIP *string
}

// InsertDeliveryAttempt persists one retry-level delivery operation.
// UNIQUE (message_id, attempt_number) makes a duplicate insert for the
// same operation number a conflict (ErrConflict) rather than silent
// double-counted history.
func (db *DB) InsertDeliveryAttempt(ctx context.Context, in NewDeliveryAttempt) (DeliveryAttempt, error) {
	if err := in.validate(); err != nil {
		return DeliveryAttempt{}, err
	}
	id, err := newID()
	if err != nil {
		return DeliveryAttempt{}, err
	}
	mxJSON, err := json.Marshal(in.MXAttempts)
	if err != nil {
		return DeliveryAttempt{}, fmt.Errorf("database: marshal mx_attempts: %w", err)
	}

	var out DeliveryAttempt
	var mxRaw []byte
	err = db.pool.QueryRow(ctx, `
		INSERT INTO delivery_attempts
			(id, message_id, attempt_number, decision, kind, accepted, final_code, enhanced_status,
			 remote_message, failure_stage, recipient, quit_error, mx_attempts, started_at, finished_at,
			 sending_member_id, transport_kind, effective_hostname, effective_source_ip)
		VALUES ($1,$2,$3,$4,$5,$6, NULLIF($7,0), NULLIF($8,''), NULLIF($9,''), NULLIF($10,''), NULLIF($11,''), NULLIF($12,''), $13,$14,$15,
			NULLIF($16,''), NULLIF($17,''), NULLIF($18,''), NULLIF($19,'')::inet)
		RETURNING id, message_id, attempt_number, decision, kind, accepted, final_code, enhanced_status,
			remote_message, failure_stage, recipient, quit_error, mx_attempts, started_at, finished_at, created_at,
			sending_member_id, transport_kind, effective_hostname, host(effective_source_ip)`,
		id, in.MessageID, in.AttemptNumber, in.Decision, in.Kind, in.Accepted,
		in.FinalCode, in.EnhancedStatus, in.RemoteMessage, in.FailureStage, in.Recipient, in.QuitError,
		mxJSON, in.StartedAt, in.FinishedAt,
		in.SendingMemberID, in.TransportKind, in.EffectiveHostname, in.EffectiveSourceIP,
	).Scan(&out.ID, &out.MessageID, &out.AttemptNumber, &out.Decision, &out.Kind, &out.Accepted,
		&out.FinalCode, &out.EnhancedStatus, &out.RemoteMessage, &out.FailureStage, &out.Recipient, &out.QuitError,
		&mxRaw, &out.StartedAt, &out.FinishedAt, &out.CreatedAt,
		&out.SendingMemberID, &out.TransportKind, &out.EffectiveHostname, &out.EffectiveSourceIP)
	if err != nil {
		return DeliveryAttempt{}, fmt.Errorf("database: insert delivery attempt: %w", normalizeErr(err))
	}
	if err := json.Unmarshal(mxRaw, &out.MXAttempts); err != nil {
		return DeliveryAttempt{}, fmt.Errorf("database: unmarshal mx_attempts: %w", err)
	}
	return out, nil
}

// ListDeliveryAttempts returns every retry-level operation for a message,
// in operation order — the future GET /emails/{id} delivery-history view.
func (db *DB) ListDeliveryAttempts(ctx context.Context, messageID string) ([]DeliveryAttempt, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, message_id, attempt_number, decision, kind, accepted, final_code, enhanced_status,
			remote_message, failure_stage, recipient, quit_error, mx_attempts, started_at, finished_at, created_at,
			sending_member_id, transport_kind, effective_hostname, host(effective_source_ip)
		FROM delivery_attempts WHERE message_id = $1 ORDER BY attempt_number`,
		messageID,
	)
	if err != nil {
		return nil, fmt.Errorf("database: list delivery attempts: %w", normalizeErr(err))
	}
	defer rows.Close()

	var out []DeliveryAttempt
	for rows.Next() {
		var a DeliveryAttempt
		var mxRaw []byte
		if err := rows.Scan(&a.ID, &a.MessageID, &a.AttemptNumber, &a.Decision, &a.Kind, &a.Accepted,
			&a.FinalCode, &a.EnhancedStatus, &a.RemoteMessage, &a.FailureStage, &a.Recipient, &a.QuitError,
			&mxRaw, &a.StartedAt, &a.FinishedAt, &a.CreatedAt,
			&a.SendingMemberID, &a.TransportKind, &a.EffectiveHostname, &a.EffectiveSourceIP); err != nil {
			return nil, fmt.Errorf("database: scan delivery attempt: %w", err)
		}
		if err := json.Unmarshal(mxRaw, &a.MXAttempts); err != nil {
			return nil, fmt.Errorf("database: unmarshal mx_attempts: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
