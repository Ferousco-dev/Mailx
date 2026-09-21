package database

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type WebhookSubscription struct {
	ID               string
	TenantID         string
	URL              string
	SecretCiphertext []byte
	SecretNonce      []byte
	EventTypes       []string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DisabledAt       *time.Time
}

type NewWebhookSubscription struct {
	TenantID         string
	URL              string
	SecretCiphertext []byte
	SecretNonce      []byte
	EventTypes       []string
}

type WebhookCursor struct {
	CreatedAt time.Time
	ID        string
}

func (db *DB) InsertWebhookSubscription(ctx context.Context, in NewWebhookSubscription) (WebhookSubscription, error) {
	if in.TenantID == "" || in.URL == "" || len(in.SecretCiphertext) == 0 || len(in.SecretNonce) == 0 || len(in.EventTypes) == 0 {
		return WebhookSubscription{}, errors.New("database: incomplete webhook subscription")
	}
	id, err := newID()
	if err != nil {
		return WebhookSubscription{}, err
	}
	var out WebhookSubscription
	err = db.pool.QueryRow(ctx, `
		INSERT INTO webhook_subscriptions
			(id, tenant_id, url, secret_ciphertext, secret_nonce, event_types)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id, tenant_id, url, secret_ciphertext, secret_nonce, event_types,
		          created_at, updated_at, disabled_at`,
		id, in.TenantID, in.URL, in.SecretCiphertext, in.SecretNonce, in.EventTypes,
	).Scan(&out.ID, &out.TenantID, &out.URL, &out.SecretCiphertext, &out.SecretNonce,
		&out.EventTypes, &out.CreatedAt, &out.UpdatedAt, &out.DisabledAt)
	if err != nil {
		return WebhookSubscription{}, fmt.Errorf("database: insert webhook subscription: %w", normalizeErr(err))
	}
	return out, nil
}

func (db *DB) GetWebhookSubscription(ctx context.Context, tenantID, id string) (WebhookSubscription, error) {
	var out WebhookSubscription
	err := db.pool.QueryRow(ctx, `
		SELECT id, tenant_id, url, secret_ciphertext, secret_nonce, event_types,
		       created_at, updated_at, disabled_at
		FROM webhook_subscriptions
		WHERE tenant_id = $1 AND id = $2 AND disabled_at IS NULL`, tenantID, id,
	).Scan(&out.ID, &out.TenantID, &out.URL, &out.SecretCiphertext, &out.SecretNonce,
		&out.EventTypes, &out.CreatedAt, &out.UpdatedAt, &out.DisabledAt)
	if err != nil {
		return WebhookSubscription{}, normalizeErr(err)
	}
	return out, nil
}

func (db *DB) ListWebhookSubscriptions(ctx context.Context, tenantID string, limit int, after *WebhookCursor) ([]WebhookSubscription, error) {
	if tenantID == "" || limit <= 0 || limit > 500 {
		return nil, errors.New("database: invalid webhook list parameters")
	}
	var afterTime *time.Time
	var afterID *string
	if after != nil {
		afterTime, afterID = &after.CreatedAt, &after.ID
	}
	rows, err := db.pool.Query(ctx, `
		SELECT id, tenant_id, url, secret_ciphertext, secret_nonce, event_types,
		       created_at, updated_at, disabled_at
		FROM webhook_subscriptions
		WHERE tenant_id = $1 AND disabled_at IS NULL
		  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3))
		ORDER BY created_at DESC, id DESC
		LIMIT $4`, tenantID, afterTime, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("database: list webhook subscriptions: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []WebhookSubscription
	for rows.Next() {
		var item WebhookSubscription
		if err := rows.Scan(&item.ID, &item.TenantID, &item.URL, &item.SecretCiphertext,
			&item.SecretNonce, &item.EventTypes, &item.CreatedAt, &item.UpdatedAt, &item.DisabledAt); err != nil {
			return nil, fmt.Errorf("database: scan webhook subscription: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (db *DB) RotateWebhookSecret(ctx context.Context, tenantID, id string, ciphertext, nonce []byte) (WebhookSubscription, error) {
	if len(ciphertext) == 0 || len(nonce) == 0 {
		return WebhookSubscription{}, errors.New("database: encrypted webhook secret is empty")
	}
	var out WebhookSubscription
	err := db.pool.QueryRow(ctx, `
		UPDATE webhook_subscriptions
		SET secret_ciphertext = $3, secret_nonce = $4, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND disabled_at IS NULL
		RETURNING id, tenant_id, url, secret_ciphertext, secret_nonce, event_types,
		          created_at, updated_at, disabled_at`, tenantID, id, ciphertext, nonce,
	).Scan(&out.ID, &out.TenantID, &out.URL, &out.SecretCiphertext, &out.SecretNonce,
		&out.EventTypes, &out.CreatedAt, &out.UpdatedAt, &out.DisabledAt)
	if err != nil {
		return WebhookSubscription{}, normalizeErr(err)
	}
	return out, nil
}

// DisableWebhookSubscription stops future fan-out and atomically cancels all
// unfinished deliveries. Historical deliveries and attempts remain queryable.
func (db *DB) DisableWebhookSubscription(ctx context.Context, tenantID, id string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin webhook disable: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE webhook_subscriptions
		SET disabled_at = now(), updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND disabled_at IS NULL`, tenantID, id)
	if err != nil {
		return fmt.Errorf("database: disable webhook: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `
		UPDATE webhook_delivery_attempts a
		SET status='failed', completed_at=now(), error_category='subscription_disabled'
		FROM webhook_deliveries d
		WHERE a.delivery_id=d.id AND a.status='in_progress'
		  AND d.tenant_id=$1 AND d.subscription_id=$2 AND d.status='delivering'`, tenantID, id); err != nil {
		return fmt.Errorf("database: close disabled webhook attempts: %w", normalizeErr(err))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE webhook_deliveries
		SET status = 'cancelled', lease_token = NULL, lease_expires_at = NULL, updated_at = now()
		WHERE tenant_id = $1 AND subscription_id = $2
		  AND status IN ('pending','delivering')`, tenantID, id); err != nil {
		return fmt.Errorf("database: cancel webhook deliveries: %w", normalizeErr(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit webhook disable: %w", normalizeErr(err))
	}
	return nil
}
