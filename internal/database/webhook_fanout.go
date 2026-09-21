package database

import (
	"context"
	"fmt"
	"time"
)

type FanOutResult struct {
	Events     int
	Deliveries int
}

// FanOutWebhookEvents establishes every delivery obligation and marks each
// event fanned out in one transaction. A crash commits both or neither.
func (db *DB) FanOutWebhookEvents(ctx context.Context, limit int) (FanOutResult, error) {
	if limit <= 0 || limit > 500 {
		return FanOutResult{}, fmt.Errorf("database: fan-out limit must be between 1 and 500")
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return FanOutResult{}, fmt.Errorf("database: begin webhook fan-out: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT id, tenant_id, event_type, occurred_at
		FROM events
		WHERE fanned_out_at IS NULL
		ORDER BY occurred_at, id
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return FanOutResult{}, fmt.Errorf("database: claim events for fan-out: %w", normalizeErr(err))
	}
	type pending struct {
		id, tenant string
		typeName   EventType
		occurredAt time.Time
	}
	var events []pending
	for rows.Next() {
		var e pending
		if err := rows.Scan(&e.id, &e.tenant, &e.typeName, &e.occurredAt); err != nil {
			rows.Close()
			return FanOutResult{}, fmt.Errorf("database: scan fan-out event: %w", err)
		}
		events = append(events, e)
	}
	rows.Close()

	result := FanOutResult{Events: len(events)}
	for _, event := range events {
		publicType, supported := PublicEventType(event.typeName)
		if supported {
			subs, err := tx.Query(ctx, `
				SELECT id
				FROM webhook_subscriptions
				WHERE tenant_id = $1 AND disabled_at IS NULL
				  AND created_at <= $2::timestamptz
				  AND $3 = ANY(event_types)`, event.tenant, event.occurredAt, publicType)
			if err != nil {
				return FanOutResult{}, fmt.Errorf("database: match webhook subscriptions: %w", normalizeErr(err))
			}
			var ids []string
			for subs.Next() {
				var id string
				if err := subs.Scan(&id); err != nil {
					subs.Close()
					return FanOutResult{}, err
				}
				ids = append(ids, id)
			}
			subs.Close()
			for _, subscriptionID := range ids {
				deliveryID, err := newID()
				if err != nil {
					return FanOutResult{}, err
				}
				tag, err := tx.Exec(ctx, `
					INSERT INTO webhook_deliveries
						(id, tenant_id, subscription_id, event_id, next_attempt_at)
					VALUES ($1,$2,$3,$4,now())
					ON CONFLICT (subscription_id, event_id) DO NOTHING`,
					deliveryID, event.tenant, subscriptionID, event.id)
				if err != nil {
					return FanOutResult{}, fmt.Errorf("database: insert webhook delivery: %w", normalizeErr(err))
				}
				result.Deliveries += int(tag.RowsAffected())
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE events SET fanned_out_at = now() WHERE id = $1`, event.id); err != nil {
			return FanOutResult{}, fmt.Errorf("database: mark event fanned out: %w", normalizeErr(err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return FanOutResult{}, fmt.Errorf("database: commit webhook fan-out: %w", normalizeErr(err))
	}
	return result, nil
}
