package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type WebhookDelivery struct {
	ID                string
	TenantID          string
	SubscriptionID    string
	EventID           string
	Status            string
	AttemptCount      int
	NextAttemptAt     time.Time
	LastErrorCategory *string
	LastResponseCode  *int
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DeliveredAt       *time.Time
	FailedAt          *time.Time
}

type WebhookDeliveryCursor struct {
	CreatedAt time.Time
	ID        string
}

type ClaimedWebhookDelivery struct {
	WebhookDelivery
	LeaseToken       string
	URL              string
	SecretCiphertext []byte
	SecretNonce      []byte
	Event            Event
}

type WebhookAttemptResult struct {
	DeliveryID      string
	LeaseToken      string
	AttemptNumber   int
	Status          string
	CompletedAt     time.Time
	Duration        time.Duration
	ResponseCode    *int
	ErrorCategory   string
	ResponseExcerpt string
	NextRetryAt     *time.Time
}

// ClaimWebhookDelivery claims one due or lease-expired delivery and creates
// its in-progress attempt record in the same transaction. At most one active
// claim per tenant is allowed, providing basic cross-tenant fairness.
func (db *DB) ClaimWebhookDelivery(ctx context.Context, now time.Time, lease time.Duration) (ClaimedWebhookDelivery, error) {
	if lease <= 0 {
		return ClaimedWebhookDelivery{}, errors.New("database: webhook lease must be positive")
	}
	// A tenant found busy after taking its claim lock is skipped so other
	// tenants' due work is still claimed in the same call.
	var busy []string
	for range maxBusyTenantSkips {
		claim, busyTenant, err := db.claimWebhookOnce(ctx, now, lease, busy)
		if busyTenant == "" {
			return claim, err
		}
		busy = append(busy, busyTenant)
	}
	return ClaimedWebhookDelivery{}, ErrNotFound
}

const maxBusyTenantSkips = 8

// claimWebhookOnce claims at most one delivery. It returns a non-empty
// busyTenant when the candidate's tenant already has an active claim that was
// committed while this transaction waited for the tenant lock.
func (db *DB) claimWebhookOnce(ctx context.Context, now time.Time, lease time.Duration, skipTenants []string) (ClaimedWebhookDelivery, string, error) {
	if skipTenants == nil {
		skipTenants = []string{}
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return ClaimedWebhookDelivery{}, "", fmt.Errorf("database: begin webhook claim: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var out ClaimedWebhookDelivery
	var metaRaw []byte
	err = tx.QueryRow(ctx, `
		SELECT d.id, d.tenant_id, d.subscription_id, d.event_id, d.status,
		       d.attempt_count, d.next_attempt_at, d.last_error_category,
		       d.last_response_code, d.created_at, d.updated_at, d.delivered_at, d.failed_at,
		       s.url, s.secret_ciphertext, s.secret_nonce,
		       e.id, e.tenant_id, e.message_id, e.event_type, e.occurred_at,
		       e.metadata, e.delivery_attempt_number
		FROM webhook_deliveries d
		JOIN webhook_subscriptions s ON s.id = d.subscription_id AND s.tenant_id = d.tenant_id
		JOIN events e ON e.id = d.event_id AND e.tenant_id = d.tenant_id
		WHERE s.disabled_at IS NULL
		  AND ((d.status = 'pending' AND d.next_attempt_at <= $1)
		       OR (d.status = 'delivering' AND d.lease_expires_at <= $1))
		  AND d.tenant_id <> ALL($2::text[])
		  AND NOT EXISTS (
		      SELECT 1 FROM webhook_deliveries active
		      WHERE active.tenant_id = d.tenant_id
		        AND active.id <> d.id
		        AND active.status = 'delivering'
		        AND active.lease_expires_at > $1
		  )
		ORDER BY d.next_attempt_at, d.created_at, d.id
		LIMIT 1
		FOR UPDATE OF d SKIP LOCKED`, now, skipTenants,
	).Scan(&out.ID, &out.TenantID, &out.SubscriptionID, &out.EventID, &out.Status,
		&out.AttemptCount, &out.NextAttemptAt, &out.LastErrorCategory, &out.LastResponseCode,
		&out.CreatedAt, &out.UpdatedAt, &out.DeliveredAt, &out.FailedAt,
		&out.URL, &out.SecretCiphertext, &out.SecretNonce,
		&out.Event.ID, &out.Event.TenantID, &out.Event.MessageID, &out.Event.Type,
		&out.Event.OccurredAt, &metaRaw, &out.Event.DeliveryAttemptNumber)
	if err != nil {
		return ClaimedWebhookDelivery{}, "", normalizeErr(err)
	}
	// Serialize per tenant: the NOT EXISTS guard above cannot see claims that
	// concurrent transactions have not committed yet. Take the tenant lock,
	// then re-check with a fresh statement, which sees anything committed
	// while we waited (READ COMMITTED).
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "mailx:webhook-claim:"+out.TenantID); err != nil {
		return ClaimedWebhookDelivery{}, "", fmt.Errorf("database: lock webhook tenant claim: %w", normalizeErr(err))
	}
	var tenantBusy bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM webhook_deliveries
		    WHERE tenant_id = $1 AND id <> $2 AND status = 'delivering' AND lease_expires_at > $3)`,
		out.TenantID, out.ID, now).Scan(&tenantBusy); err != nil {
		return ClaimedWebhookDelivery{}, "", fmt.Errorf("database: recheck webhook tenant claim: %w", normalizeErr(err))
	}
	if tenantBusy {
		return ClaimedWebhookDelivery{}, out.TenantID, nil
	}
	if err := json.Unmarshal(metaRaw, &out.Event.Metadata); err != nil {
		return ClaimedWebhookDelivery{}, "", fmt.Errorf("database: decode webhook event metadata: %w", err)
	}
	if out.Status == "delivering" {
		if _, err := tx.Exec(ctx, `
			UPDATE webhook_delivery_attempts
			SET status='retrying', completed_at=$2, error_category='lease_expired',
			    next_retry_at=$2
			WHERE delivery_id=$1 AND attempt_number=$3 AND status='in_progress'`,
			out.ID, now, out.AttemptCount); err != nil {
			return ClaimedWebhookDelivery{}, "", fmt.Errorf("database: close expired webhook attempt: %w", normalizeErr(err))
		}
	}
	token, err := newID()
	if err != nil {
		return ClaimedWebhookDelivery{}, "", err
	}
	attemptID, err := newID()
	if err != nil {
		return ClaimedWebhookDelivery{}, "", err
	}
	out.AttemptCount++
	out.LeaseToken = token
	if _, err := tx.Exec(ctx, `
		UPDATE webhook_deliveries
		SET status = 'delivering', attempt_count = $2, lease_token = $3,
		    lease_expires_at = $4, updated_at = now()
		WHERE id = $1`, out.ID, out.AttemptCount, token, now.Add(lease)); err != nil {
		return ClaimedWebhookDelivery{}, "", fmt.Errorf("database: claim webhook delivery: %w", normalizeErr(err))
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO webhook_delivery_attempts
			(id, delivery_id, attempt_number, status, attempted_at)
		VALUES ($1,$2,$3,'in_progress',$4)`, attemptID, out.ID, out.AttemptCount, now); err != nil {
		return ClaimedWebhookDelivery{}, "", fmt.Errorf("database: start webhook attempt: %w", normalizeErr(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return ClaimedWebhookDelivery{}, "", fmt.Errorf("database: commit webhook claim: %w", normalizeErr(err))
	}
	return out, "", nil
}

// CompleteWebhookAttempt atomically records the HTTP result and transitions
// the delivery. lease_token fences stale workers after lease reclamation.
func (db *DB) CompleteWebhookAttempt(ctx context.Context, in WebhookAttemptResult) error {
	if in.DeliveryID == "" || in.LeaseToken == "" || in.AttemptNumber <= 0 {
		return errors.New("database: incomplete webhook attempt result")
	}
	if in.Status != "succeeded" && in.Status != "retrying" && in.Status != "failed" {
		return errors.New("database: invalid webhook attempt status")
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin webhook completion: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var attemptCount int
	if err := tx.QueryRow(ctx, `
		SELECT attempt_count FROM webhook_deliveries
		WHERE id = $1 AND status = 'delivering' AND lease_token = $2
		FOR UPDATE`, in.DeliveryID, in.LeaseToken).Scan(&attemptCount); err != nil {
		return normalizeErr(err)
	}
	if attemptCount != in.AttemptNumber {
		return ErrConflict
	}
	tag, err := tx.Exec(ctx, `
		UPDATE webhook_delivery_attempts
		SET status = $3, completed_at = $4, duration_ms = $5,
		    response_code = $6, error_category = NULLIF($7,''),
		    response_excerpt = NULLIF($8,''), next_retry_at = $9
		WHERE delivery_id = $1 AND attempt_number = $2 AND status = 'in_progress'`,
		in.DeliveryID, in.AttemptNumber, in.Status, in.CompletedAt,
		in.Duration.Milliseconds(), in.ResponseCode, in.ErrorCategory,
		in.ResponseExcerpt, in.NextRetryAt)
	if err != nil {
		return fmt.Errorf("database: complete webhook attempt: %w", normalizeErr(err))
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	switch in.Status {
	case "succeeded":
		_, err = tx.Exec(ctx, `
			UPDATE webhook_deliveries SET status='succeeded', delivered_at=$3,
			    lease_token=NULL, lease_expires_at=NULL, last_error_category=NULL,
			    last_response_code=$4, updated_at=now()
			WHERE id=$1 AND lease_token=$2`, in.DeliveryID, in.LeaseToken, in.CompletedAt, in.ResponseCode)
	case "retrying":
		if in.NextRetryAt == nil {
			return errors.New("database: retrying webhook attempt has no schedule")
		}
		_, err = tx.Exec(ctx, `
			UPDATE webhook_deliveries SET status='pending', next_attempt_at=$3,
			    lease_token=NULL, lease_expires_at=NULL, last_error_category=NULLIF($4,''),
			    last_response_code=$5, updated_at=now()
			WHERE id=$1 AND lease_token=$2`, in.DeliveryID, in.LeaseToken,
			*in.NextRetryAt, in.ErrorCategory, in.ResponseCode)
	case "failed":
		_, err = tx.Exec(ctx, `
			UPDATE webhook_deliveries SET status='failed', failed_at=$3,
			    lease_token=NULL, lease_expires_at=NULL, last_error_category=NULLIF($4,''),
			    last_response_code=$5, updated_at=now()
			WHERE id=$1 AND lease_token=$2`, in.DeliveryID, in.LeaseToken,
			in.CompletedAt, in.ErrorCategory, in.ResponseCode)
	}
	if err != nil {
		return fmt.Errorf("database: transition webhook delivery: %w", normalizeErr(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit webhook completion: %w", normalizeErr(err))
	}
	return nil
}

func (db *DB) ListWebhookDeliveries(ctx context.Context, tenantID, subscriptionID string, limit int) ([]WebhookDelivery, error) {
	return db.ListWebhookDeliveriesPage(ctx, tenantID, subscriptionID, limit, nil)
}

func (db *DB) ListWebhookDeliveriesPage(ctx context.Context, tenantID, subscriptionID string, limit int, after *WebhookDeliveryCursor) ([]WebhookDelivery, error) {
	if limit <= 0 || limit > 500 {
		return nil, errors.New("database: invalid webhook delivery limit")
	}
	var afterTime *time.Time
	var afterID *string
	if after != nil {
		afterTime = &after.CreatedAt
		afterID = &after.ID
	}
	rows, err := db.pool.Query(ctx, `
		SELECT d.id, d.tenant_id, d.subscription_id, d.event_id, d.status,
		       d.attempt_count, d.next_attempt_at, d.last_error_category,
		       d.last_response_code, d.created_at, d.updated_at, d.delivered_at, d.failed_at
		FROM webhook_deliveries d
		JOIN webhook_subscriptions s ON s.id=d.subscription_id AND s.tenant_id=d.tenant_id
		WHERE d.tenant_id=$1 AND d.subscription_id=$2
		  AND ($3::timestamptz IS NULL OR (d.created_at, d.id) < ($3, $4))
		ORDER BY d.created_at DESC, d.id DESC LIMIT $5`, tenantID, subscriptionID, afterTime, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("database: list webhook deliveries: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []WebhookDelivery
	for rows.Next() {
		var d WebhookDelivery
		if err := rows.Scan(&d.ID, &d.TenantID, &d.SubscriptionID, &d.EventID, &d.Status,
			&d.AttemptCount, &d.NextAttemptAt, &d.LastErrorCategory, &d.LastResponseCode,
			&d.CreatedAt, &d.UpdatedAt, &d.DeliveredAt, &d.FailedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
