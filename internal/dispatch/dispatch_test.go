package dispatch

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/queue"
)

type fakeOutbox struct {
	mu         sync.Mutex
	items      map[string]database.OutboxItem
	dispatched map[string]bool
}

func newFakeOutbox() *fakeOutbox {
	return &fakeOutbox{items: map[string]database.OutboxItem{}, dispatched: map[string]bool{}}
}

func (f *fakeOutbox) add(item database.OutboxItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[item.MessageID] = item
}

func (f *fakeOutbox) ListPendingOutbox(_ context.Context, now time.Time, limit int) ([]database.OutboxItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []database.OutboxItem
	for id, item := range f.items {
		if !f.dispatched[id] && !item.AvailableAt.After(now) {
			out = append(out, item)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeOutbox) MarkOutboxDispatched(_ context.Context, messageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.items[messageID]; !ok {
		return database.ErrNotFound
	}
	if f.dispatched[messageID] {
		return database.ErrNotFound
	}
	f.dispatched[messageID] = true
	return nil
}

func mustQueue(t *testing.T) *queue.MemoryQueue {
	t.Helper()
	q, err := queue.NewMemoryQueue(100)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestTickDispatchesDueItemAndMarksIt(t *testing.T) {
	ob := newFakeOutbox()
	ob.add(database.OutboxItem{MessageID: "m1", TenantID: "t1", AvailableAt: time.Now()})
	q := mustQueue(t)
	d := New(ob, q)

	d.tick(context.Background())

	claim, err := q.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if claim.Job.ID != "m1" {
		t.Fatalf("got %+v", claim.Job)
	}
	if !ob.dispatched["m1"] {
		t.Fatal("expected outbox row marked dispatched")
	}
}

func TestTickSkipsNotYetDueItems(t *testing.T) {
	ob := newFakeOutbox()
	ob.add(database.OutboxItem{MessageID: "future", TenantID: "t1", AvailableAt: time.Now().Add(time.Hour)})
	q := mustQueue(t)
	d := New(ob, q)

	d.tick(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := q.Claim(ctx); err == nil {
		t.Fatal("future-scheduled item must not be dispatched yet")
	}
}

// Simulates a crash after Enqueue succeeded but before MarkOutboxDispatched
// committed: the next tick must re-run without creating a second logical
// job, relying on v0.16's duplicate-active-JobID Enqueue semantics.
func TestTickRedispatchAfterCrashBeforeMarkIsDuplicateSafe(t *testing.T) {
	ob := newFakeOutbox()
	ob.add(database.OutboxItem{MessageID: "m1", TenantID: "t1", AvailableAt: time.Now()})
	q := mustQueue(t)
	d := New(ob, q)

	// First tick's Enqueue succeeds, but we skip its mark to model the crash.
	if err := q.Enqueue(context.Background(), queue.Job{ID: "m1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}

	d.tick(context.Background()) // recovery tick, as if nothing had happened yet

	if q.Len() != 1 {
		t.Fatalf("expected exactly one logical job after redispatch, got %d", q.Len())
	}
	if !ob.dispatched["m1"] {
		t.Fatal("recovery tick must still mark the row dispatched")
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	ob := newFakeOutbox()
	q := mustQueue(t)
	d := New(ob, q, WithInterval(10*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run must return nil on context cancellation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}
}
