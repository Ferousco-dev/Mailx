// Package worker is MailX's bounded worker pool: it claims queue.Job
// values and drives the existing retry/delivery stack to completion for
// each one, Ack'ing on a terminal outcome or Release'ing for a later
// retry. It runs exactly Config.Workers goroutines — no goroutine-per-job,
// no unbounded channel — and inherits internal/queue's at-least-once (not
// exactly-once) delivery guarantee.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/observability"
	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/retry"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

var ErrInvalidWorkerCount = errors.New("worker: worker count must be positive")

const (
	bookkeepingTimeout = 5 * time.Second
	// claimRetryDelay is the pause after a failed queue claim.
	claimRetryDelay = time.Second
)

// Loader and Coordinator are narrow interfaces so tests can substitute
// deterministic fakes; *storage.FileStore and *retry.Coordinator satisfy
// them in production.
type Loader interface {
	Load(id string) (storage.StoredMessage, error)
}

type Coordinator interface {
	Attempt(ctx context.Context, state *retry.State, req delivery.Request, now time.Time) (retry.Outcome, error)
}

// Config has no default Workers value — callers must choose deliberately.
type Config struct {
	Workers      int
	ReportingMTA string
}

type Option func(*Pool)

func WithClock(now func() time.Time) Option {
	return func(p *Pool) { p.now = now }
}

// WithOnError reports non-fatal per-job errors; one bad job never stops
// the pool. Unset means errors are silently discarded.
func WithOnError(fn func(error)) Option {
	return func(p *Pool) { p.onError = fn }
}

// WithLogger installs a structured logger. Records carry job/message IDs and
// bounded categories only (never recipients, domains, or remote SMTP text).
func WithLogger(l *slog.Logger) Option {
	return func(p *Pool) {
		if l != nil {
			p.log = l
		}
	}
}

// WithMetrics installs the aggregate metrics sink; nil disables metrics.
func WithMetrics(m *observability.Metrics) Option {
	return func(p *Pool) { p.metrics = m }
}

type Pool struct {
	log          *slog.Logger
	metrics      *observability.Metrics
	q            queue.Queue
	loader       Loader
	coordinator  Coordinator
	outcomes     OutcomeStore
	workers      int
	reportingMTA string
	now          func() time.Time
	onError      func(error)
	states       *stateStore
	wg           sync.WaitGroup
}

func NewPool(q queue.Queue, loader Loader, coordinator Coordinator, outcomes OutcomeStore, cfg Config, opts ...Option) (*Pool, error) {
	if q == nil {
		return nil, errors.New("worker: queue must not be nil")
	}
	if loader == nil {
		return nil, errors.New("worker: loader must not be nil")
	}
	if coordinator == nil {
		return nil, errors.New("worker: coordinator must not be nil")
	}
	if outcomes == nil {
		return nil, errors.New("worker: outcome store must not be nil")
	}
	if cfg.Workers <= 0 {
		return nil, ErrInvalidWorkerCount
	}
	reportingMTA := cfg.ReportingMTA
	if reportingMTA == "" {
		reportingMTA = "mailx.local"
	}
	p := &Pool{
		q:            q,
		loader:       loader,
		coordinator:  coordinator,
		outcomes:     outcomes,
		workers:      cfg.Workers,
		reportingMTA: reportingMTA,
		now:          func() time.Time { return time.Now().UTC() },
		onError:      func(error) {},
		log:          observability.Discard(),
		states:       newStateStore(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// Run blocks until ctx is canceled and every worker has exited; it always
// returns nil since shutdown is the expected path, not a failure.
func (p *Pool) Run(ctx context.Context) error {
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go p.runWorker(ctx)
	}
	p.wg.Wait()
	return nil
}

func (p *Pool) runWorker(ctx context.Context) {
	defer p.wg.Done()
	for {
		// Claim has a non-blocking fast path that ignores ctx when a job
		// is already available, so shutdown must be checked explicitly here.
		if err := ctx.Err(); err != nil {
			return
		}
		c, err := p.q.Claim(ctx)
		if err != nil {
			// Shutdown or a closed queue ends the worker. Any other error is a
			// transient backend outage (e.g. Redis down): keep the worker alive,
			// back off, and resume when the queue recovers, so a dependency
			// outage never takes the whole process (and its liveness) down.
			if ctx.Err() != nil || errors.Is(err, queue.ErrQueueClosed) {
				return
			}
			p.metrics.QueueOp("claim", err)
			p.onError(fmt.Errorf("worker: claim: %w", err))
			p.log.Error("queue_claim_failed")
			timer := time.NewTimer(claimRetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		p.metrics.QueueOp("claim", nil)
		p.log.Debug("queue_claimed", "job_id", c.Job.ID, "message_id", c.Job.MessageID)
		p.safeProcess(ctx, c)
	}
}

// safeProcess recovers a panic from one job's processing, reports it, and
// releases the claim rather than leaking it (queue claims have no lease).
func (p *Pool) safeProcess(ctx context.Context, c queue.Claim) {
	defer func() {
		if r := recover(); r != nil {
			p.onError(fmt.Errorf("worker: recovered panic processing job %s: %v", c.Job.ID, r))
			p.log.Error("worker_panic", "job_id", c.Job.ID, "message_id", c.Job.MessageID)
			// Once final DATA was accepted, releasing here would knowingly make
			// the same SMTP transmission eligible again. Keep the claim; Redis
			// lease recovery plus durable-state loading handles a later reclaim.
			if state, ok := p.states.lookup(c.Job.ID); ok {
				if latest, exists := state.Latest(); exists && latest.Result.Accepted {
					return
				}
			}
			p.release(c, p.now())
		}
	}()
	p.processOne(ctx, c)
}

// release uses a fresh context so bookkeeping still completes after the
// pool's own shutdown context has been canceled.
func (p *Pool) release(c queue.Claim, availableAt time.Time) bool {
	ctx, cancel := context.WithTimeout(context.Background(), bookkeepingTimeout)
	defer cancel()
	err := p.q.Release(ctx, c.Job.ID, c.Token, availableAt)
	p.metrics.QueueOp("release", err)
	if err != nil {
		p.onError(fmt.Errorf("worker: release job %s: %w", c.Job.ID, err))
		p.log.Error("queue_release_failed", "job_id", c.Job.ID, "message_id", c.Job.MessageID)
		return false
	}
	p.log.Info("queue_released", "job_id", c.Job.ID, "message_id", c.Job.MessageID, "available_at", availableAt.UTC())
	return true
}

func (p *Pool) ack(c queue.Claim) bool {
	ctx, cancel := context.WithTimeout(context.Background(), bookkeepingTimeout)
	defer cancel()
	err := p.q.Ack(ctx, c.Job.ID, c.Token)
	p.metrics.QueueOp("ack", err)
	if err != nil {
		p.onError(fmt.Errorf("worker: ack job %s: %w", c.Job.ID, err))
		p.log.Error("queue_ack_failed", "job_id", c.Job.ID, "message_id", c.Job.MessageID)
		return false
	}
	p.log.Info("queue_acked", "job_id", c.Job.ID, "message_id", c.Job.MessageID)
	return true
}
