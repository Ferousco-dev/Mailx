package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrBroadcastStateConflict = errors.New("database: broadcast cannot be changed in its current state")
var ErrInvalidBroadcastSchedule = errors.New("database: broadcast send_at must be in the future")

func (db *DB) CancelBroadcast(ctx context.Context, tenantID, id string) (Broadcast, error) {
	tx, b, err := db.lockBroadcast(ctx, tenantID, id)
	if err != nil {
		return Broadcast{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if b.Status == "cancelled" {
		return b, nil
	}
	if b.Status != "accepted" && b.Status != "expanding" {
		return Broadcast{}, ErrBroadcastStateConflict
	}
	b, err = scanBroadcast(tx.QueryRow(ctx, `
		UPDATE broadcasts SET status = 'cancelled', completed_at = clock_timestamp(), updated_at = clock_timestamp()
		WHERE tenant_id = $1 AND id = $2 RETURNING `+broadcastColumns, tenantID, id))
	if err != nil {
		return Broadcast{}, fmt.Errorf("database: cancel broadcast: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Broadcast{}, fmt.Errorf("database: commit broadcast cancellation: %w", normalizeErr(err))
	}
	return b, nil
}

func (db *DB) RescheduleBroadcast(ctx context.Context, tenantID, id string, sendAt time.Time) (Broadcast, error) {
	tx, b, err := db.lockBroadcast(ctx, tenantID, id)
	if err != nil {
		return Broadcast{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if b.Status != "accepted" {
		return Broadcast{}, ErrBroadcastStateConflict
	}
	b, err = scanBroadcast(tx.QueryRow(ctx, `
		UPDATE broadcasts SET send_at = $3, updated_at = clock_timestamp()
		WHERE tenant_id = $1 AND id = $2 AND $3::timestamptz > clock_timestamp()
		RETURNING `+broadcastColumns, tenantID, id, sendAt))
	if errors.Is(err, ErrNotFound) {
		return Broadcast{}, ErrInvalidBroadcastSchedule
	}
	if err != nil {
		return Broadcast{}, fmt.Errorf("database: reschedule broadcast: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Broadcast{}, fmt.Errorf("database: commit broadcast reschedule: %w", normalizeErr(err))
	}
	return b, nil
}

func (db *DB) lockBroadcast(ctx context.Context, tenantID, id string) (pgx.Tx, Broadcast, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, Broadcast{}, fmt.Errorf("database: begin broadcast change: %w", normalizeErr(err))
	}
	b, err := scanBroadcast(tx.QueryRow(ctx, `SELECT `+broadcastColumns+`
		FROM broadcasts WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, id))
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, Broadcast{}, fmt.Errorf("database: lock broadcast: %w", err)
	}
	return tx, b, nil
}

func lockBroadcastRecipient(ctx context.Context, tx pgx.Tx, in NewMessage) error {
	var broadcastID, broadcastStatus, recipientStatus string
	var due bool
	err := tx.QueryRow(ctx, `SELECT broadcast_id FROM broadcast_recipients
		WHERE tenant_id = $1 AND id = $2`, in.TenantID, in.RequireBroadcastRecipientID).Scan(&broadcastID)
	if errors.Is(normalizeErr(err), ErrNotFound) {
		return ErrRecipientErased
	}
	if err != nil {
		return fmt.Errorf("database: find broadcast recipient: %w", normalizeErr(err))
	}
	err = tx.QueryRow(ctx, `SELECT status, send_at IS NULL OR send_at <= clock_timestamp() FROM broadcasts
		WHERE tenant_id = $1 AND id = $2 FOR SHARE`, in.TenantID, broadcastID).Scan(&broadcastStatus, &due)
	if err != nil {
		return fmt.Errorf("database: lock recipient broadcast: %w", normalizeErr(err))
	}
	err = tx.QueryRow(ctx, `SELECT status FROM broadcast_recipients
		WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, in.TenantID, in.RequireBroadcastRecipientID).Scan(&recipientStatus)
	if errors.Is(normalizeErr(err), ErrNotFound) {
		return ErrRecipientErased
	}
	if err != nil {
		return fmt.Errorf("database: lock broadcast recipient: %w", normalizeErr(err))
	}
	var materialized bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM messages WHERE tenant_id = $1 AND id = $2)`,
		in.TenantID, in.ID).Scan(&materialized); err != nil {
		return fmt.Errorf("database: check materialized recipient: %w", normalizeErr(err))
	}
	if materialized {
		return ErrConflict
	}
	if (broadcastStatus != "accepted" && broadcastStatus != "expanding") || !due || recipientStatus != "pending" {
		return ErrBroadcastStateConflict
	}
	return nil
}
