package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/retry"
)

type orderedQueue struct {
	queue.Queue
	mu          sync.Mutex
	order       []string
	releaseAt   []time.Time
	notify      chan struct{}
	failAck     bool
	failRelease bool
}

func (q *orderedQueue) mark(value string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.order = append(q.order, value)
	if q.notify != nil {
		select {
		case q.notify <- struct{}{}:
		default:
		}
	}
}

func (q *orderedQueue) waitFor(t *testing.T, count int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		q.mu.Lock()
		got := len(q.order)
		q.mu.Unlock()
		if got >= count {
			return
		}
		select {
		case <-q.notify:
		case <-deadline:
			t.Fatalf("timed out waiting for %d queue operations, got %d", count, got)
		}
	}
}

func (q *orderedQueue) Ack(ctx context.Context, id string, token uint64) error {
	q.mark("ack")
	if q.failAck {
		return errors.New("controlled ack failure")
	}
	return q.Queue.Ack(ctx, id, token)
}

func (q *orderedQueue) Release(ctx context.Context, id string, token uint64, at time.Time) error {
	q.mark("release")
	if q.failRelease {
		return errors.New("controlled release failure")
	}
	q.mu.Lock()
	q.releaseAt = append(q.releaseAt, at)
	q.mu.Unlock()
	return q.Queue.Release(ctx, id, token, at)
}

func TestOutcomePersistsBeforeQueueFinalization(t *testing.T) {
	tests := []struct {
		name    string
		outcome retry.Outcome
		last    string
	}{
		{"success", retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true, Kind: delivery.KindAccepted}}, "ack"},
		{"temporary", retry.Outcome{Status: retry.StatusRetryable, Result: delivery.Result{Kind: delivery.KindTransferTemporary}, Schedule: &retry.Schedule{NextRetryAt: time.Now().Add(time.Hour)}}, "release"},
		{"failure", retry.Outcome{Status: retry.StatusFailed, Result: delivery.Result{Kind: delivery.KindTransferPermanent}}, "ack"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := mustQ(t, 2)
			q := &orderedQueue{Queue: base, notify: make(chan struct{}, 8)}
			loader := newFakeLoader()
			loader.put("m1", envelopeFor("<a@x>", "<b@y>"), "x\r\n")
			coord := newScriptedCoordinator()
			coord.script("<a@x>", coordOutcome{outcome: tc.outcome})
			store := newFakeOutcomeStore()
			store.persist = func(context.Context, string, retry.DeliveryAttempt, retry.Outcome) error {
				q.mark("persist")
				return nil
			}
			p, err := NewPool(q, loader, coord, store, Config{Workers: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { _ = p.Run(ctx); close(done) }()
			q.waitFor(t, 2)
			cancel()
			<-done
			q.mu.Lock()
			defer q.mu.Unlock()
			if len(q.order) < 2 || q.order[0] != "persist" || q.order[1] != tc.last {
				t.Fatalf("outcome order = %v; want persist then %s", q.order, tc.last)
			}
		})
	}
}

func TestAcceptedPersistenceFailureRetriesDatabaseNotSMTP(t *testing.T) {
	base := mustQ(t, 2)
	loader := newFakeLoader()
	loader.put("m1", envelopeFor("<a@x>", "<b@y>"), "x\r\n")
	coord := newScriptedCoordinator()
	coord.script("<a@x>", coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true, Kind: delivery.KindAccepted}}})
	store := newFakeOutcomeStore()
	var calls atomic.Int32
	store.persist = func(context.Context, string, retry.DeliveryAttempt, retry.Outcome) error {
		if calls.Add(1) == 1 {
			return errors.New("controlled database failure")
		}
		return nil
	}
	p, err := NewPool(base, loader, coord, store, Config{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := base.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	deadline := time.After(3 * time.Second)
	for base.Len() != 0 {
		select {
		case <-deadline:
			t.Fatal("outcome was not eventually persisted and acked")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done
	if calls.Load() < 2 || coord.totalAttempts() != 1 {
		t.Fatalf("persist calls=%d SMTP attempts=%d; wanted DB retry without SMTP retry", calls.Load(), coord.totalAttempts())
	}
}

func TestAcceptedPersistenceFailureOnShutdownDoesNotRelease(t *testing.T) {
	base := mustQ(t, 2)
	q := &orderedQueue{Queue: base, notify: make(chan struct{}, 8)}
	loader := newFakeLoader()
	loader.put("m1", envelopeFor("<a@x>", "<b@y>"), "x\r\n")
	coord := newScriptedCoordinator()
	coord.script("<a@x>", coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true, Kind: delivery.KindAccepted}}})
	store := newFakeOutcomeStore()
	called := make(chan struct{}, 1)
	store.persist = func(context.Context, string, retry.DeliveryAttempt, retry.Outcome) error {
		select {
		case called <- struct{}{}:
		default:
		}
		return errors.New("database unavailable")
	}
	p, err := NewPool(q, loader, coord, store, Config{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("persistence was not attempted")
	}
	cancel()
	<-done
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.order) != 0 || base.Len() != 1 || coord.totalAttempts() != 1 {
		t.Fatalf("accepted outcome was finalized or retransmitted: queue ops=%v len=%d attempts=%d", q.order, base.Len(), coord.totalAttempts())
	}
}

func TestPanicDuringAcceptedOutcomePersistenceDoesNotRelease(t *testing.T) {
	base := mustQ(t, 2)
	q := &orderedQueue{Queue: base, notify: make(chan struct{}, 8)}
	loader := newFakeLoader()
	loader.put("m1", envelopeFor("<a@x>", "<b@y>"), "x\r\n")
	coord := newScriptedCoordinator()
	coord.script("<a@x>", coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true, Kind: delivery.KindAccepted}}})
	store := newFakeOutcomeStore()
	called := make(chan struct{})
	store.persist = func(context.Context, string, retry.DeliveryAttempt, retry.Outcome) error {
		close(called)
		panic("controlled persistence panic")
	}
	p, err := NewPool(q, loader, coord, store, Config{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("persistence path was not reached")
	}
	cancel()
	<-done
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.order) != 0 || base.Len() != 1 || coord.totalAttempts() != 1 {
		t.Fatalf("persistence panic made accepted delivery eligible again: ops=%v len=%d SMTP=%d", q.order, base.Len(), coord.totalAttempts())
	}
}

func TestDurableTerminalStateSkipsSMTPAfterReclaim(t *testing.T) {
	q := mustQ(t, 2)
	loader := newFakeLoader()
	coord := newScriptedCoordinator()
	store := newFakeOutcomeStore()
	store.terminal["m1"] = true
	p, err := NewPool(q, loader, coord, store, Config{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	deadline := time.After(2 * time.Second)
	for q.Len() != 0 {
		select {
		case <-deadline:
			t.Fatal("terminal reclaimed job was not acked")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done
	if coord.totalAttempts() != 0 || atomic.LoadInt32(&loader.calls) != 0 {
		t.Fatalf("durable terminal job reached work path: SMTP=%d loads=%d", coord.totalAttempts(), loader.calls)
	}
}

func TestConcurrentTerminalPersistenceAcksInsteadOfRelease(t *testing.T) {
	base := mustQ(t, 2)
	q := &orderedQueue{Queue: base, notify: make(chan struct{}, 8)}
	loader := newFakeLoader()
	loader.put("m1", envelopeFor("<a@x>", "<b@y>"), "x\r\n")
	coord := newScriptedCoordinator()
	coord.script("<a@x>", coordOutcome{outcome: retry.Outcome{
		Status:   retry.StatusRetryable,
		Result:   delivery.Result{Kind: delivery.KindTransferTemporary},
		Schedule: &retry.Schedule{NextRetryAt: time.Now().Add(time.Hour)},
	}})
	store := newFakeOutcomeStore()
	store.persist = func(context.Context, string, retry.DeliveryAttempt, retry.Outcome) error {
		return ErrOutcomeAlreadyTerminal
	}
	p, err := NewPool(q, loader, coord, store, Config{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	q.waitFor(t, 1)
	cancel()
	<-done
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.order) != 1 || q.order[0] != "ack" || base.Len() != 0 {
		t.Fatalf("concurrent terminal outcome did not stop retry: ops=%v len=%d", q.order, base.Len())
	}
}

func TestQueueAckFailureLeavesDurableTerminalProtection(t *testing.T) {
	base := mustQ(t, 2)
	q := &orderedQueue{Queue: base, notify: make(chan struct{}, 8), failAck: true}
	loader := newFakeLoader()
	loader.put("m1", envelopeFor("<a@x>", "<b@y>"), "x\r\n")
	coord := newScriptedCoordinator()
	coord.script("<a@x>", coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true, Kind: delivery.KindAccepted}}})
	store := newFakeOutcomeStore()
	p, err := NewPool(q, loader, coord, store, Config{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	go func() { _ = p.Run(ctx1); close(done1) }()
	q.waitFor(t, 1)
	cancel1()
	<-done1

	// Simulate lease recovery/restart with a fresh queue and pool. The durable
	// terminal state must Ack without a second SMTP operation.
	q2 := mustQ(t, 2)
	if err := q2.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	p2, err := NewPool(q2, loader, coord, store, Config{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p2.Run(ctx); close(done) }()
	deadline := time.After(2 * time.Second)
	for q2.Len() != 0 {
		select {
		case <-deadline:
			t.Fatal("reclaimed terminal job was not acked")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done
	if coord.totalAttempts() != 1 {
		t.Fatalf("Ack failure caused SMTP retransmission: attempts=%d", coord.totalAttempts())
	}
}

func TestQueueReleaseFailureUsesDurableRetryStateAfterReclaim(t *testing.T) {
	firstTime := time.Now().UTC().Add(time.Hour)
	base := mustQ(t, 2)
	q := &orderedQueue{Queue: base, notify: make(chan struct{}, 8), failRelease: true}
	loader := newFakeLoader()
	loader.put("m1", envelopeFor("<a@x>", "<b@y>"), "x\r\n")
	coord := newScriptedCoordinator()
	coord.script("<a@x>",
		coordOutcome{outcome: retry.Outcome{Status: retry.StatusRetryable, Result: delivery.Result{Kind: delivery.KindTransferTemporary}, Schedule: &retry.Schedule{NextRetryAt: firstTime}}},
		coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true, Kind: delivery.KindAccepted}}},
	)
	store := newFakeOutcomeStore()
	p, err := NewPool(q, loader, coord, store, Config{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	go func() { _ = p.Run(ctx1); close(done1) }()
	q.waitFor(t, 1)
	cancel1()
	<-done1

	base2 := mustQ(t, 2)
	q2 := &orderedQueue{Queue: base2, notify: make(chan struct{}, 8)}
	if err := q2.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	p2, err := NewPool(q2, loader, coord, store, Config{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p2.Run(ctx); close(done) }()
	q2.waitFor(t, 1)
	cancel()
	<-done
	q2.mu.Lock()
	if len(q2.releaseAt) != 1 || !q2.releaseAt[0].Equal(firstTime) {
		t.Fatalf("reclaim did not restore durable retry schedule: %v", q2.releaseAt)
	}
	q2.mu.Unlock()
	if coord.totalAttempts() != 1 {
		t.Fatalf("reclaim retried SMTP before durable schedule: attempts=%d", coord.totalAttempts())
	}

	q3 := mustQ(t, 2)
	if err := q3.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	p3, err := NewPool(q3, loader, coord, store, Config{Workers: 1}, WithClock(func() time.Time { return firstTime }))
	if err != nil {
		t.Fatal(err)
	}
	ctx3, cancel3 := context.WithCancel(context.Background())
	done3 := make(chan struct{})
	go func() { _ = p3.Run(ctx3); close(done3) }()
	deadline := time.After(2 * time.Second)
	for q3.Len() != 0 {
		select {
		case <-deadline:
			t.Fatal("retry did not complete when durable schedule became due")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel3()
	<-done3
	store.mu.Lock()
	defer store.mu.Unlock()
	if coord.totalAttempts() != 2 || len(store.attempts["m1"]) != 2 || store.attempts["m1"][1].Number != 2 {
		t.Fatalf("durable retry state not resumed: SMTP=%d attempts=%+v", coord.totalAttempts(), store.attempts["m1"])
	}
}
