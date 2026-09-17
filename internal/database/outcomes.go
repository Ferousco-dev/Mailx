package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrMessageTerminal means a new delivery operation was presented for a
// message whose terminal outcome is already durable. Callers must not send the
// message again; a duplicate persistence call for the same attempt is instead
// treated as a successful idempotent replay.
var ErrMessageTerminal = errors.New("database: message delivery is terminal")

// DeliveryState is the durable worker-facing delivery state. Attempts are
// ordered by attempt number and Status is the authoritative message status.
type DeliveryState struct {
	Status      MessageStatus
	Attempts    []DeliveryAttempt
	NextRetryAt *time.Time
}

// LoadDeliveryState reconstructs the minimum durable truth a worker needs
// before deciding whether another SMTP operation is safe.
func (db *DB) LoadDeliveryState(ctx context.Context, messageID string) (DeliveryState, error) {
	var state DeliveryState
	if err := db.pool.QueryRow(ctx, `SELECT status FROM messages WHERE id = $1`, messageID).Scan(&state.Status); err != nil {
		return DeliveryState{}, normalizeErr(err)
	}
	attempts, err := db.ListDeliveryAttempts(ctx, messageID)
	if err != nil {
		return DeliveryState{}, err
	}
	state.Attempts = attempts
	if state.Status == StatusRetrying {
		var next *time.Time
		err := db.pool.QueryRow(ctx, `
			SELECT (metadata->>'next_retry_at')::timestamptz
			FROM events
			WHERE message_id = $1
			  AND event_type = 'deferred'
			  AND delivery_attempt_number IS NOT NULL
			ORDER BY delivery_attempt_number DESC
			LIMIT 1`, messageID,
		).Scan(&next)
		if err != nil {
			return DeliveryState{}, fmt.Errorf("database: load delivery retry schedule: %w", normalizeErr(err))
		}
		state.NextRetryAt = next
	}
	return state, nil
}

// Terminal reports whether SMTP must never be attempted again for this
// durable message state.
func (s DeliveryState) Terminal() bool {
	switch s.Status {
	case StatusDelivered, StatusFailed, StatusBounced:
		return true
	default:
		return false
	}
}

// PersistDeliveryOutcome atomically inserts one retry-level attempt, updates
// the authoritative message status, and appends its immutable outcome event.
// Repeating the exact attempt number is an idempotent success; presenting a
// new attempt after terminal truth is durable returns ErrMessageTerminal.
func (db *DB) PersistDeliveryOutcome(ctx context.Context, in NewDeliveryAttempt, retryExhausted bool, metadata map[string]any) error {
	if err := in.validate(); err != nil {
		return err
	}
	status, eventType, deliveredAt, err := classifyDeliveryOutcome(in, retryExhausted)
	if err != nil {
		return err
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["attempt_number"] = in.AttemptNumber
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("database: marshal delivery outcome event: %w", err)
	}
	mxJSON, err := json.Marshal(in.MXAttempts)
	if err != nil {
		return fmt.Errorf("database: marshal delivery outcome MX attempts: %w", err)
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin delivery outcome: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current MessageStatus
	var tenantID string
	if err := tx.QueryRow(ctx, `SELECT tenant_id, status FROM messages WHERE id = $1 FOR UPDATE`, in.MessageID).Scan(&tenantID, &current); err != nil {
		return fmt.Errorf("database: lock delivery message: %w", normalizeErr(err))
	}

	duplicate, err := matchingAttemptExists(ctx, tx, in)
	if err != nil {
		return err
	}
	if terminalMessageStatus(current) && !duplicate {
		return ErrMessageTerminal
	}

	if !duplicate {
		attemptID, err := newID()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO delivery_attempts
				(id, message_id, attempt_number, decision, kind, accepted, final_code, enhanced_status,
				 remote_message, failure_stage, recipient, quit_error, mx_attempts, started_at, finished_at)
			VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,0),NULLIF($8,''),NULLIF($9,''),NULLIF($10,''),NULLIF($11,''),NULLIF($12,''),$13,$14,$15)`,
			attemptID, in.MessageID, in.AttemptNumber, in.Decision, in.Kind, in.Accepted,
			in.FinalCode, in.EnhancedStatus, in.RemoteMessage, in.FailureStage, in.Recipient,
			in.QuitError, mxJSON, in.StartedAt, in.FinishedAt,
		); err != nil {
			return fmt.Errorf("database: insert delivery outcome attempt: %w", normalizeErr(err))
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE messages
		SET status = $1, updated_at = now(), delivered_at = COALESCE($2, delivered_at)
		WHERE id = $3`, status, deliveredAt, in.MessageID,
	); err != nil {
		return fmt.Errorf("database: update delivery outcome status: %w", normalizeErr(err))
	}

	eventID, err := newID()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO events
			(id, tenant_id, message_id, event_type, metadata, delivery_attempt_number)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (message_id, delivery_attempt_number)
		WHERE delivery_attempt_number IS NOT NULL DO NOTHING`,
		eventID, tenantID, in.MessageID, eventType, metaJSON, in.AttemptNumber,
	); err != nil {
		return fmt.Errorf("database: insert delivery outcome event: %w", normalizeErr(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit delivery outcome: %w", normalizeErr(err))
	}
	return nil
}

func classifyDeliveryOutcome(in NewDeliveryAttempt, retryExhausted bool) (MessageStatus, EventType, *time.Time, error) {
	switch in.Decision {
	case DecisionRetry:
		if in.Accepted {
			return "", "", nil, errors.New("database: accepted delivery cannot be retryable")
		}
		if retryExhausted {
			return StatusFailed, EventFailed, nil, nil
		}
		return StatusRetrying, EventDeferred, nil, nil
	case DecisionTerminalSuccess:
		if retryExhausted {
			return "", "", nil, errors.New("database: terminal success cannot be retry exhausted")
		}
		if !in.Accepted {
			return "", "", nil, errors.New("database: terminal success must preserve Accepted=true")
		}
		finished := in.FinishedAt
		return StatusDelivered, EventDelivered, &finished, nil
	case DecisionTerminalFailure:
		if retryExhausted {
			return "", "", nil, errors.New("database: terminal failure cannot be retry exhausted")
		}
		if in.Accepted {
			return "", "", nil, errors.New("database: accepted delivery cannot be terminal failure")
		}
		return StatusFailed, EventFailed, nil, nil
	default:
		return "", "", nil, fmt.Errorf("database: invalid delivery decision %q", in.Decision)
	}
}

func terminalMessageStatus(status MessageStatus) bool {
	return DeliveryState{Status: status}.Terminal()
}

func matchingAttemptExists(ctx context.Context, tx pgx.Tx, in NewDeliveryAttempt) (bool, error) {
	var decision Decision
	var kind string
	var accepted bool
	var finalCode *int
	err := tx.QueryRow(ctx, `
		SELECT decision, kind, accepted, final_code
		FROM delivery_attempts
		WHERE message_id = $1 AND attempt_number = $2`, in.MessageID, in.AttemptNumber,
	).Scan(&decision, &kind, &accepted, &finalCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("database: inspect existing delivery attempt: %w", normalizeErr(err))
	}
	code := 0
	if finalCode != nil {
		code = *finalCode
	}
	if decision != in.Decision || kind != in.Kind || accepted != in.Accepted || code != in.FinalCode {
		return false, fmt.Errorf("database: conflicting delivery attempt %s/%d: %w", in.MessageID, in.AttemptNumber, ErrConflict)
	}
	return true, nil
}
