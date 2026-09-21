package webhook

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
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
	return &WorkerPool{db: db, box: box, client: client, config: cfg, onError: onError}, nil
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
		p.safeDeliver(ctx, claim)
	}
}

func (p *WorkerPool) safeDeliver(ctx context.Context, claim database.ClaimedWebhookDelivery) {
	defer func() {
		if recovered := recover(); recovered != nil {
			p.onError(fmt.Errorf("webhook: recovered panic delivering %s: %v", claim.ID, recovered))
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
		// No destructive fallback: the lease remains durable and reclaimable.
	}
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
