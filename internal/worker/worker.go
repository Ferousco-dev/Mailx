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
	"sync"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/retry"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

var ErrInvalidWorkerCount = errors.New("worker: worker count must be positive")

const bookkeepingTimeout = 5 * time.Second

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

// WithStatusReporter reports each job's lifecycle outcome (message ID,
// retry status, and — only for StatusSucceeded — when delivery finished)
// so a caller can durably persist it (e.g. internal/database.
// UpdateMessageStatus) without worker itself depending on any storage
// backend. Best-effort: called synchronously, after Ack/Release has
// already happened, so a reporter failure never blocks queue progress.
func WithStatusReporter(fn func(ctx context.Context, messageID string, status retry.LifecycleStatus, deliveredAt time.Time)) Option {
	return func(p *Pool) { p.statusFn = fn }
}

type Pool struct {
	q            queue.Queue
	loader       Loader
	coordinator  Coordinator
	workers      int
	reportingMTA string
	now          func() time.Time
	onError      func(error)
	statusFn     func(ctx context.Context, messageID string, status retry.LifecycleStatus, deliveredAt time.Time)
	states       *stateStore
	wg           sync.WaitGroup
}

func NewPool(q queue.Queue, loader Loader, coordinator Coordinator, cfg Config, opts ...Option) (*Pool, error) {
	if q == nil {
		return nil, errors.New("worker: queue must not be nil")
	}
	if loader == nil {
		return nil, errors.New("worker: loader must not be nil")
	}
	if coordinator == nil {
		return nil, errors.New("worker: coordinator must not be nil")
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
		workers:      cfg.Workers,
		reportingMTA: reportingMTA,
		now:          func() time.Time { return time.Now().UTC() },
		onError:      func(error) {},
		statusFn:     func(context.Context, string, retry.LifecycleStatus, time.Time) {},
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
			return
		}
		p.safeProcess(ctx, c)
	}
}

// safeProcess recovers a panic from one job's processing, reports it, and
// releases the claim rather than leaking it (queue claims have no lease).
func (p *Pool) safeProcess(ctx context.Context, c queue.Claim) {
	defer func() {
		if r := recover(); r != nil {
			p.onError(fmt.Errorf("worker: recovered panic processing job %s: %v", c.Job.ID, r))
			p.releaseBestEffort(c, p.now())
		}
	}()
	p.processOne(ctx, c)
}

// releaseBestEffort uses a fresh context so bookkeeping still completes
// after the pool's own shutdown context has been canceled.
func (p *Pool) releaseBestEffort(c queue.Claim, availableAt time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), bookkeepingTimeout)
	defer cancel()
	if err := p.q.Release(ctx, c.Job.ID, c.Token, availableAt); err != nil {
		p.onError(fmt.Errorf("worker: release job %s: %w", c.Job.ID, err))
	}
}

func (p *Pool) ackBestEffort(c queue.Claim) {
	ctx, cancel := context.WithTimeout(context.Background(), bookkeepingTimeout)
	defer cancel()
	if err := p.q.Ack(ctx, c.Job.ID, c.Token); err != nil {
		p.onError(fmt.Errorf("worker: ack job %s: %w", c.Job.ID, err))
	}
}
