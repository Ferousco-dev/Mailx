// Package dispatch closes the durable-acceptance gap between v0.15's
// PostgreSQL outbox and v0.16's Redis queue: it is the recovery
// mechanism that makes "PostgreSQL COMMIT, then crash before Redis
// Enqueue" survivable, by re-scanning the outbox on every tick rather
// than only dispatching at the moment of acceptance.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/queue"
)

const (
	defaultInterval  = time.Second
	defaultBatchSize = 100
)

// Outbox is the narrow database dependency Dispatcher needs, so tests can
// substitute a fake without a real PostgreSQL connection.
type Outbox interface {
	ListPendingOutbox(ctx context.Context, now time.Time, limit int) ([]database.OutboxItem, error)
	MarkOutboxDispatched(ctx context.Context, messageID string) error
}

type Option func(*Dispatcher)

func WithInterval(d time.Duration) Option { return func(disp *Dispatcher) { disp.interval = d } }
func WithBatchSize(n int) Option          { return func(disp *Dispatcher) { disp.batchSize = n } }

// WithOnError reports non-fatal per-item errors; one bad row never stops
// the dispatcher. Unset means errors are silently discarded.
func WithOnError(fn func(error)) Option { return func(disp *Dispatcher) { disp.onError = fn } }

// Dispatcher polls the outbox and hands due rows to a queue.Queue.
type Dispatcher struct {
	outbox    Outbox
	q         queue.Queue
	now       func() time.Time
	interval  time.Duration
	batchSize int
	onError   func(error)
}

func New(outbox Outbox, q queue.Queue, opts ...Option) *Dispatcher {
	d := &Dispatcher{
		outbox: outbox, q: q,
		now:       func() time.Time { return time.Now().UTC() },
		interval:  defaultInterval,
		batchSize: defaultBatchSize,
		onError:   func(error) {},
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Run polls until ctx is canceled. It always returns nil: shutdown via
// context cancellation is the expected path, not a failure.
func (d *Dispatcher) Run(ctx context.Context) error {
	for {
		d.tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(d.interval):
		}
	}
}

// tick dispatches one batch. Enqueue is duplicate-safe by JobID (v0.16),
// and MarkOutboxDispatched is a guarded no-op the second time (v0.18), so
// a crash between the two on a previous tick is always safely retried
// here rather than needing its own recovery path.
func (d *Dispatcher) tick(ctx context.Context) {
	items, err := d.outbox.ListPendingOutbox(ctx, d.now(), d.batchSize)
	if err != nil {
		d.onError(fmt.Errorf("dispatch: list pending outbox: %w", err))
		return
	}
	for _, item := range items {
		if err := d.q.Enqueue(ctx, queue.Job{ID: item.MessageID, MessageID: item.MessageID, AvailableAt: item.AvailableAt}); err != nil {
			d.onError(fmt.Errorf("dispatch: enqueue %s: %w", item.MessageID, err))
			continue
		}
		if err := d.outbox.MarkOutboxDispatched(ctx, item.MessageID); err != nil && !errors.Is(err, database.ErrNotFound) {
			d.onError(fmt.Errorf("dispatch: mark dispatched %s: %w", item.MessageID, err))
		}
	}
}
