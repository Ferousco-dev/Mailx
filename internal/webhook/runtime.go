package webhook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/observability"
)

type FanOut struct {
	db       *database.DB
	interval time.Duration
	onError  func(error)
}

func NewFanOut(db *database.DB, onError func(error)) *FanOut {
	if onError == nil {
		onError = func(error) {}
	}
	return &FanOut{db: db, interval: 250 * time.Millisecond, onError: onError}
}

func (f *FanOut) Run(ctx context.Context) error {
	for {
		result, err := f.db.FanOutWebhookEvents(ctx, 100)
		if err != nil && ctx.Err() == nil {
			f.onError(err)
		}
		wait := f.interval
		if err == nil && result.Events == 100 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

type WorkerConfig struct {
	Workers      int
	ClaimLease   time.Duration
	PollInterval time.Duration
}

type WorkerPool struct {
	db      *database.DB
	box     *SecretBox
	client  *Client
	config  WorkerConfig
	onError func(error)
	wg      sync.WaitGroup
	log     *slog.Logger
	metrics *observability.Metrics
}

// WithObservability installs structured logging and metrics. Records carry
// tenant/subscription/event/delivery IDs and bounded categories only: never
// the URL, secret, signature, or response body.
func (p *WorkerPool) WithObservability(l *slog.Logger, m *observability.Metrics) *WorkerPool {
	if l != nil {
		p.log = l
	}
	p.metrics = m
	return p
}

func NewWorkerPool(db *database.DB, box *SecretBox, client *Client, cfg WorkerConfig, onError func(error)) (*WorkerPool, error) {
	if db == nil || box == nil || client == nil {
		return nil, errors.New("webhook: worker dependencies are required")
	}
	if cfg.Workers <= 0 {
		return nil, errors.New("webhook: worker count must be positive")
	}
	if cfg.ClaimLease <= 0 {
		cfg.ClaimLease = 30 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 250 * time.Millisecond
	}
	if onError == nil {
		onError = func(error) {}
	}
	return &WorkerPool{db: db, box: box, client: client, config: cfg, onError: onError, log: observability.Discard()}, nil
}

func (p *WorkerPool) Run(ctx context.Context) error {
	defer p.client.CloseIdleConnections()
	for range p.config.Workers {
		p.wg.Add(1)
		go p.worker(ctx)
	}
	p.wg.Wait()
	return nil
}

func (p *WorkerPool) worker(ctx context.Context) {
	defer p.wg.Done()
	for ctx.Err() == nil {
		claim, err := p.db.ClaimWebhookDelivery(ctx, time.Now().UTC(), p.config.ClaimLease)
		if errors.Is(err, database.ErrNotFound) {
			if !waitContext(ctx, p.config.PollInterval) {
				return
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.onError(fmt.Errorf("webhook claim: %w", err))
			if !waitContext(ctx, p.config.PollInterval) {
				return
			}
			continue
		}
		p.log.Debug("webhook_claimed", "delivery_id", claim.ID, "subscription_id", claim.SubscriptionID,
			"event_id", claim.Event.ID, "tenant_id", claim.TenantID, "attempt", claim.AttemptCount)
		if claim.Status == "delivering" {
			p.log.Warn("webhook_reclaimed", "delivery_id", claim.ID, "subscription_id", claim.SubscriptionID,
				"event_id", claim.Event.ID, "tenant_id", claim.TenantID, "attempt", claim.AttemptCount)
		}
		p.safeDeliver(ctx, claim)
	}
}

func (p *WorkerPool) safeDeliver(ctx context.Context, claim database.ClaimedWebhookDelivery) {
	defer func() {
		if recovered := recover(); recovered != nil {
			p.onError(fmt.Errorf("webhook: recovered panic delivering %s: %v", claim.ID, recovered))
			p.log.Error("webhook_panic", "delivery_id", claim.ID, "subscription_id", claim.SubscriptionID)
			// The durable lease expires; another worker will reclaim the already
			// started attempt without losing the delivery obligation.
		}
	}()
	secret, err := p.box.Decrypt(claim.SecretCiphertext, claim.SecretNonce, []byte(claim.TenantID))
	if err != nil {
		p.complete(claim, AttemptOutcome{Status: "failed", ErrorCategory: "secret_decrypt"})
		return
	}
	outcome := p.client.Deliver(ctx, claim, secret)
	p.complete(claim, outcome)
}

func (p *WorkerPool) complete(claim database.ClaimedWebhookDelivery, outcome AttemptOutcome) {
	completed := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := p.db.CompleteWebhookAttempt(ctx, database.WebhookAttemptResult{
		DeliveryID: claim.ID, LeaseToken: claim.LeaseToken, AttemptNumber: claim.AttemptCount,
		Status: outcome.Status, CompletedAt: completed, Duration: outcome.Duration,
		ResponseCode: outcome.ResponseCode, ErrorCategory: outcome.ErrorCategory,
		NextRetryAt: outcome.NextRetryAt,
	})
	if err != nil {
		p.onError(fmt.Errorf("webhook complete %s: %w", claim.ID, err))
		p.log.Error("webhook_complete_failed", "delivery_id", claim.ID, "subscription_id", claim.SubscriptionID)
		// No destructive fallback: the lease remains durable and reclaimable.
		return
	}
	p.metrics.WebhookAttempt(outcome.Status, outcome.Duration)
	level := slog.LevelInfo
	if outcome.Status != "succeeded" {
		level = slog.LevelWarn
	}
	attrs := []any{"delivery_id", claim.ID, "subscription_id", claim.SubscriptionID, "event_id", claim.Event.ID,
		"tenant_id", claim.TenantID, "attempt", claim.AttemptCount, "outcome", outcome.Status,
		"duration_ms", outcome.Duration.Milliseconds()}
	if outcome.ResponseCode != nil {
		attrs = append(attrs, "response_code", *outcome.ResponseCode)
	}
	if outcome.ErrorCategory != "" {
		attrs = append(attrs, "error_category", outcome.ErrorCategory)
	}
	if outcome.NextRetryAt != nil {
		attrs = append(attrs, "next_retry_at", outcome.NextRetryAt.UTC())
	}
	p.log.Log(context.Background(), level, "webhook_attempt", attrs...)
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
