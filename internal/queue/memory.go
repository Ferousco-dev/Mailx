// In-memory Queue implementation. All state is lost on process restart,
// including in-flight claims — this gives at-least-once processing while
// the process is alive, never exactly-once (see queue.go).
package queue

import (
	"container/heap"
	"context"
	"sync"
	"time"
)

type entry struct {
	job     Job
	seq     uint64
	claimed bool
	token   uint64
}

// entryHeap orders unclaimed entries by AvailableAt, then seq for FIFO.
type entryHeap []*entry

func (h entryHeap) Len() int { return len(h) }
func (h entryHeap) Less(i, j int) bool {
	ai, aj := h[i].job.AvailableAt, h[j].job.AvailableAt
	if !ai.Equal(aj) {
		return ai.Before(aj)
	}
	return h[i].seq < h[j].seq
}
func (h entryHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *entryHeap) Push(x any)   { *h = append(*h, x.(*entry)) }
func (h *entryHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return e
}

// MemoryQueue is a bounded, race-safe, single-process Queue.
type MemoryQueue struct {
	mu       sync.Mutex
	capacity int
	now      func() time.Time
	closed   bool

	entries map[string]*entry
	avail   entryHeap

	seq       uint64
	nextToken uint64

	// notify is closed and replaced under mu on any state change waiters
	// should recheck; chosen over sync.Cond so ctx.Done() can interrupt a wait.
	notify chan struct{}
}

type Option func(*MemoryQueue)

func WithClock(now func() time.Time) Option {
	return func(q *MemoryQueue) { q.now = now }
}

// NewMemoryQueue bounds ACTIVE jobs (available + claimed) to capacity;
// Enqueue blocks past that bound until an Ack frees a slot.
func NewMemoryQueue(capacity int, opts ...Option) (*MemoryQueue, error) {
	if capacity <= 0 {
		return nil, ErrInvalidCapacity
	}
	q := &MemoryQueue{
		capacity: capacity,
		now:      func() time.Time { return time.Now().UTC() },
		entries:  make(map[string]*entry),
		notify:   make(chan struct{}),
	}
	for _, opt := range opts {
		opt(q)
	}
	return q, nil
}

func (q *MemoryQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries)
}

func (q *MemoryQueue) broadcastLocked() {
	close(q.notify)
	q.notify = make(chan struct{})
}

var _ Queue = (*MemoryQueue)(nil)

func (q *MemoryQueue) Enqueue(ctx context.Context, job Job) error {
	if err := job.validate(); err != nil {
		return err
	}
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return ErrQueueClosed
		}
		if _, exists := q.entries[job.ID]; exists {
			q.mu.Unlock()
			return nil
		}
		if len(q.entries) < q.capacity {
			normalized := job
			if normalized.EnqueuedAt.IsZero() {
				normalized.EnqueuedAt = q.now()
			}
			if normalized.AvailableAt.IsZero() {
				normalized.AvailableAt = normalized.EnqueuedAt
			}
			q.seq++
			e := &entry{job: normalized, seq: q.seq}
			q.entries[job.ID] = e
			heap.Push(&q.avail, e)
			q.broadcastLocked()
			q.mu.Unlock()
			return nil
		}
		ch := q.notify
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

func (q *MemoryQueue) Claim(ctx context.Context) (Claim, error) {
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return Claim{}, ErrQueueClosed
		}
		if len(q.avail) > 0 {
			top := q.avail[0]
			now := q.now()
			if !top.job.AvailableAt.After(now) {
				heap.Pop(&q.avail)
				q.nextToken++
				top.claimed = true
				top.token = q.nextToken
				result := Claim{Job: top.job, Token: top.token}
				q.mu.Unlock()
				return result, nil
			}
			wait := top.job.AvailableAt.Sub(now)
			ch := q.notify
			q.mu.Unlock()

			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return Claim{}, ctx.Err()
			case <-ch:
				timer.Stop()
			case <-timer.C:
			}
			continue
		}
		ch := q.notify
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return Claim{}, ctx.Err()
		case <-ch:
		}
	}
}

func (q *MemoryQueue) Ack(ctx context.Context, id string, token uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.entries[id]
	if !ok {
		return ErrUnknownJob
	}
	if !e.claimed || e.token != token {
		return ErrJobNotClaimed
	}
	delete(q.entries, id)
	q.broadcastLocked()
	return nil
}

func (q *MemoryQueue) Renew(ctx context.Context, id string, token uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.entries[id]
	if !ok {
		return ErrUnknownJob
	}
	if !e.claimed || e.token != token {
		return ErrJobNotClaimed
	}
	return nil
}

func (q *MemoryQueue) Release(ctx context.Context, id string, token uint64, availableAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.entries[id]
	if !ok {
		return ErrUnknownJob
	}
	if !e.claimed || e.token != token {
		return ErrJobNotClaimed
	}
	e.claimed = false
	e.job.AvailableAt = availableAt
	q.seq++
	e.seq = q.seq
	heap.Push(&q.avail, e)
	q.broadcastLocked()
	return nil
}

func (q *MemoryQueue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil
	}
	q.closed = true
	q.broadcastLocked()
	return nil
}
