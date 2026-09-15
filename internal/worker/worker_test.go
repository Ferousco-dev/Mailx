package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/retry"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

// ------------------------------------------------------------ fakes -----

// fakeLoader returns scripted StoredMessage values or an error, by
// MessageID.
type fakeLoader struct {
	mu       sync.Mutex
	messages map[string]storage.StoredMessage
	errs     map[string]error
	calls    int32
	// notify, if non-nil, receives one value after every Load call so
	// tests can wait deterministically instead of polling.
	notify chan struct{}
}

func newFakeLoader() *fakeLoader {
	return &fakeLoader{messages: map[string]storage.StoredMessage{}, errs: map[string]error{}, notify: make(chan struct{}, 1024)}
}

func (f *fakeLoader) Load(id string) (storage.StoredMessage, error) {
	atomic.AddInt32(&f.calls, 1)
	defer func() {
		select {
		case f.notify <- struct{}{}:
		default:
		}
	}()
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.errs[id]; ok {
		return storage.StoredMessage{}, err
	}
	if m, ok := f.messages[id]; ok {
		return m, nil
	}
	return storage.StoredMessage{}, fmt.Errorf("fakeLoader: no message %q", id)
}

func (f *fakeLoader) put(id string, envelope storage.StoredEnvelope, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages[id] = storage.StoredMessage{
		Metadata: storage.StoredMessageMetadata{ID: id, Envelope: envelope},
		Raw:      []byte(raw),
	}
}

func (f *fakeLoader) putErr(id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[id] = err
}

// scriptedCoordinator returns pre-scripted Outcomes per call, in order,
// per job — tracked by a caller-supplied key function (usually
// req.Envelope.MailFrom or a fixed key for single-job tests). It also
// tracks the maximum number of concurrently in-flight Attempt calls, for
// proving bounded worker concurrency.
type scriptedCoordinator struct {
	mu      sync.Mutex
	scripts map[string][]coordOutcome
	idx     map[string]int

	current  int32
	maxSeen  int32
	attempts int32

	// delay, if set, makes Attempt block until ctx.Done or this duration
	// elapses — used to hold a job "in flight" for concurrency/shutdown
	// tests. barrier, if set, is closed by the test once it has observed
	// the desired number of concurrent Attempt calls.
	delay   time.Duration
	barrier chan struct{}

	// notify, if non-nil, receives one value after every completed
	// Attempt call so tests can wait deterministically instead of polling.
	notify chan struct{}
}

type coordOutcome struct {
	outcome retry.Outcome
	err     error
	panic   any // if non-nil, Attempt panics with this value instead
}

func newScriptedCoordinator() *scriptedCoordinator {
	return &scriptedCoordinator{scripts: map[string][]coordOutcome{}, idx: map[string]int{}, notify: make(chan struct{}, 1024)}
}

// waitAttempts blocks until at least n Attempt calls have completed, or
// fails the test after timeout. Uses the notify channel, never sleep.
func (c *scriptedCoordinator) waitAttempts(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for c.totalAttempts() < n {
		select {
		case <-c.notify:
		case <-deadline:
			t.Fatalf("timed out waiting for %d attempts, got %d", n, c.totalAttempts())
		}
	}
}

func (c *scriptedCoordinator) script(key string, outcomes ...coordOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scripts[key] = outcomes
}

func (c *scriptedCoordinator) Attempt(ctx context.Context, state *retry.State, req delivery.Request, now time.Time) (retry.Outcome, error) {
	// Mirror the real retry.Coordinator.Attempt contract: an already-
	// canceled context short-circuits BEFORE any delivery work happens,
	// and is not counted as a real delivery attempt.
	if err := ctx.Err(); err != nil {
		return retry.Outcome{}, err
	}
	defer func() {
		atomic.AddInt32(&c.attempts, 1)
		select {
		case c.notify <- struct{}{}:
		default:
		}
	}()
	cur := atomic.AddInt32(&c.current, 1)
	defer atomic.AddInt32(&c.current, -1)
	for {
		prevMax := atomic.LoadInt32(&c.maxSeen)
		if cur <= prevMax || atomic.CompareAndSwapInt32(&c.maxSeen, prevMax, cur) {
			break
		}
	}

	if c.delay > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(c.delay):
		}
	}
	if c.barrier != nil {
		<-c.barrier
	}

	key := req.Envelope.MailFrom
	c.mu.Lock()
	outcomes := c.scripts[key]
	i := c.idx[key]
	c.idx[key] = i + 1
	c.mu.Unlock()

	if i >= len(outcomes) {
		return retry.Outcome{}, fmt.Errorf("scriptedCoordinator: no outcome #%d scripted for key %q", i, key)
	}
	o := outcomes[i]
	if o.panic != nil {
		panic(o.panic)
	}
	if o.err == nil {
		// Mirror retry.Coordinator's real behavior: a normal attempt
		// records itself into state. Tests that care about state history
		// (e.g. proving it persists across Claim/Release/Claim) depend on
		// this to be meaningful.
		_ = state.Record(o.outcome.Result, o.outcome.DeliveryError)
	}
	return o.outcome, o.err
}

func (c *scriptedCoordinator) maxConcurrent() int { return int(atomic.LoadInt32(&c.maxSeen)) }
func (c *scriptedCoordinator) totalAttempts() int { return int(atomic.LoadInt32(&c.attempts)) }

// ---------------------------------------------------------- helpers -----

func mustPool(t *testing.T, q queue.Queue, l Loader, c Coordinator, cfg Config, opts ...Option) *Pool {
	t.Helper()
	p, err := NewPool(q, l, c, cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func envelopeFor(mailFrom string, recipients ...string) storage.StoredEnvelope {
	return storage.StoredEnvelope{MailFrom: mailFrom, RcptTo: recipients}
}

func collectErrors() (*func(error), *[]error, *sync.Mutex) {
	var mu sync.Mutex
	var errs []error
	fn := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		errs = append(errs, err)
	}
	return &fn, &errs, &mu
}

// runPoolUntilIdle starts the pool, blocks until the given coordinator has
// completed wantAttempts calls (deterministic, via its notify channel),
// then cancels and waits for Run to return.
func runPoolUntilIdle(t *testing.T, p *Pool, q *queue.MemoryQueue, timeout time.Duration) {
	t.Helper()
	runPoolForAttempts(t, p, q, 1, timeout)
}

func runPoolForAttempts(t *testing.T, p *Pool, q *queue.MemoryQueue, wantAttempts int, timeout time.Duration) {
	t.Helper()
	c, ok := p.coordinator.(*scriptedCoordinator)
	if !ok {
		t.Fatal("runPoolForAttempts requires a *scriptedCoordinator")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	c.waitAttempts(t, wantAttempts, timeout)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not shut down after cancel")
	}
}

// ------------------------------------------------------------ NewPool ---

func TestNewPoolValidation(t *testing.T) {
	q := mustQ(t, 4)
	l := newFakeLoader()
	c := newScriptedCoordinator()

	if _, err := NewPool(nil, l, c, Config{Workers: 1}); err == nil {
		t.Fatal("nil queue must be rejected")
	}
	if _, err := NewPool(q, nil, c, Config{Workers: 1}); err == nil {
		t.Fatal("nil loader must be rejected")
	}
	if _, err := NewPool(q, l, nil, Config{Workers: 1}); err == nil {
		t.Fatal("nil coordinator must be rejected")
	}
	for _, n := range []int{0, -1, -100} {
		if _, err := NewPool(q, l, c, Config{Workers: n}); !errors.Is(err, ErrInvalidWorkerCount) {
			t.Fatalf("workers=%d: expected ErrInvalidWorkerCount, got %v", n, err)
		}
	}
	if _, err := NewPool(q, l, c, Config{Workers: 1}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if _, err := NewPool(q, l, c, Config{Workers: 10000}); err != nil {
		t.Fatalf("large worker count rejected: %v", err)
	}
}

func mustQ(t *testing.T, capacity int, opts ...queue.Option) *queue.MemoryQueue {
	t.Helper()
	q, err := queue.NewMemoryQueue(capacity, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// -------------------------------------------------- single-worker path --

func TestSingleWorkerSuccessAcks(t *testing.T) {
	q := mustQ(t, 4)
	l := newFakeLoader()
	l.put("m1", envelopeFor("<a@x>", "<b@y>"), "Subject: x\r\n\r\nbody\r\n")
	c := newScriptedCoordinator()
	c.script("<a@x>", coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true, Kind: delivery.KindAccepted}}})

	errFn, errs, mu := collectErrors()
	p := mustPool(t, q, l, c, Config{Workers: 1}, WithOnError(*errFn))

	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	runPoolUntilIdle(t, p, q, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(*errs) != 0 {
		t.Fatalf("unexpected operational errors: %v", *errs)
	}
	if q.Len() != 0 {
		t.Fatalf("job must be acked (removed), Len=%d", q.Len())
	}
	if c.totalAttempts() != 1 {
		t.Fatalf("expected exactly 1 attempt, got %d", c.totalAttempts())
	}
}

func TestSingleWorkerTemporaryReleasesAtExactSchedule(t *testing.T) {
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := mustQ(t, 4, queue.WithClock(func() time.Time { return frozen }))
	l := newFakeLoader()
	l.put("m1", envelopeFor("<temp@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator()
	schedule := retry.Schedule{Attempt: 1, Delay: 30 * time.Minute, NextRetryAt: frozen.Add(30 * time.Minute)}
	c.script("<temp@x>", coordOutcome{outcome: retry.Outcome{
		Status:   retry.StatusRetryable,
		Result:   delivery.Result{Kind: delivery.KindTransferTemporary, FinalCode: 451},
		Schedule: &schedule,
	}})

	p := mustPool(t, q, l, c, Config{Workers: 1}, WithClock(func() time.Time { return frozen }))
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}

	runPoolForAttempts(t, p, q, 1, 2*time.Second)

	if c.totalAttempts() != 1 {
		t.Fatalf("expected exactly 1 attempt, got %d", c.totalAttempts())
	}
	if q.Len() != 1 {
		t.Fatalf("temporary failure must release (keep tracked), Len=%d", q.Len())
	}
	// Must not be claimable before the scheduled time.
	tooSoon, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	if _, err := q.Claim(tooSoon); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("job became available before NextRetryAt: %v", err)
	}
}

func TestSingleWorkerPermanentFailureAcks(t *testing.T) {
	q := mustQ(t, 4)
	l := newFakeLoader()
	l.put("m1", envelopeFor("<perm@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator()
	c.script("<perm@x>", coordOutcome{outcome: retry.Outcome{
		Status: retry.StatusFailed,
		Result: delivery.Result{Kind: delivery.KindTransferPermanent, FinalCode: 550, FailureStage: string(smtp.StageRcptTo)},
	}})

	p := mustPool(t, q, l, c, Config{Workers: 1})
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	runPoolUntilIdle(t, p, q, 2*time.Second)

	if q.Len() != 0 {
		t.Fatalf("permanent failure must be acked, Len=%d", q.Len())
	}
	if c.totalAttempts() != 1 {
		t.Fatalf("expected exactly 1 attempt, got %d", c.totalAttempts())
	}
}

func TestSingleWorkerExhaustionAcksNoThirdAttempt(t *testing.T) {
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := mustQ(t, 4, queue.WithClock(func() time.Time { return frozen }))
	l := newFakeLoader()
	l.put("m1", envelopeFor("<exh@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator()
	sched1 := retry.Schedule{Attempt: 1, Delay: time.Minute, NextRetryAt: frozen.Add(time.Minute)}
	c.script("<exh@x>",
		coordOutcome{outcome: retry.Outcome{Status: retry.StatusRetryable, Result: delivery.Result{Kind: delivery.KindTransferTemporary, FinalCode: 451}, Schedule: &sched1}},
		coordOutcome{outcome: retry.Outcome{Status: retry.StatusExhausted, Result: delivery.Result{Kind: delivery.KindTransferTemporary, FinalCode: 451}}},
	)

	p := mustPool(t, q, l, c, Config{Workers: 1}, WithClock(func() time.Time { return frozen }))
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}

	runPoolForAttempts(t, p, q, 1, 2*time.Second)
	if c.totalAttempts() != 1 || q.Len() != 1 {
		t.Fatalf("expected 1 attempt + released job, got attempts=%d Len=%d", c.totalAttempts(), q.Len())
	}

	// Re-home onto a queue whose clock reports the scheduled time (see
	// v0.13's own clock-injection pattern for this reasoning).
	q2 := mustQ(t, 4, queue.WithClock(func() time.Time { return sched1.NextRetryAt }))
	if err := q2.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1", AvailableAt: sched1.NextRetryAt}); err != nil {
		t.Fatal(err)
	}
	p2 := mustPool(t, q2, l, c, Config{Workers: 1}, WithClock(func() time.Time { return sched1.NextRetryAt }))
	p2.states = p.states // simulate "same process, job re-claimed" by sharing state store

	// c is shared with p's earlier attempt, so wait for the CUMULATIVE
	// total to reach 2, not for "1 more" (which would already be true).
	runPoolForAttempts(t, p2, q2, 2, 2*time.Second)
	if c.totalAttempts() != 2 {
		t.Fatalf("expected exactly 2 total attempts (exhaustion, no 3rd), got %d", c.totalAttempts())
	}
	if q2.Len() != 0 {
		t.Fatalf("exhausted job must be acked, Len=%d", q2.Len())
	}
}

func TestSingleWorkerAcceptedWithQuitErrorStillAcksNoRelease(t *testing.T) {
	q := mustQ(t, 4)
	l := newFakeLoader()
	l.put("m1", envelopeFor("<q@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator()
	c.script("<q@x>", coordOutcome{outcome: retry.Outcome{
		Status: retry.StatusSucceeded,
		Result: delivery.Result{Accepted: true, Kind: delivery.KindAccepted, FinalCode: 250, QuitError: "connection reset during QUIT"},
	}})

	p := mustPool(t, q, l, c, Config{Workers: 1})
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	runPoolUntilIdle(t, p, q, 2*time.Second)

	if q.Len() != 0 {
		t.Fatalf("accepted+QUIT-error must still ack, Len=%d", q.Len())
	}
	if c.totalAttempts() != 1 {
		t.Fatalf("accepted+QUIT-error must NOT trigger a second attempt, got %d", c.totalAttempts())
	}
}

// ------------------------------------------------------- retry-state ---

// TestRetryStatePersistsAcrossClaimReleaseClaim is the critical test named
// explicitly in the milestone: retry.State must NOT be recreated empty on
// every Claim, or exhaustion could never occur.
func TestRetryStatePersistsAcrossClaimReleaseClaim(t *testing.T) {
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := mustQ(t, 4, queue.WithClock(func() time.Time { return frozen }))
	l := newFakeLoader()
	l.put("m1", envelopeFor("<r@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator()
	limit := retry.AttemptLimit{MaxAttempts: 2}
	sched1 := retry.Schedule{Attempt: 1, Delay: time.Minute, NextRetryAt: frozen.Add(time.Minute)}
	c.script("<r@x>",
		coordOutcome{outcome: retry.Outcome{Status: retry.StatusRetryable, Result: delivery.Result{Kind: delivery.KindTransferTemporary, FinalCode: 451}, Schedule: &sched1}},
		coordOutcome{outcome: retry.Outcome{Status: retry.StatusExhausted, Result: delivery.Result{Kind: delivery.KindTransferTemporary, FinalCode: 451}}},
	)
	_ = limit // limit enforcement lives in retry.Coordinator itself; scriptedCoordinator here only proves worker preserves state, not that it computes exhaustion.

	p := mustPool(t, q, l, c, Config{Workers: 1}, WithClock(func() time.Time { return frozen }))
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	if p.states.len() != 0 {
		t.Fatalf("state store must start empty, got %d", p.states.len())
	}

	runPoolForAttempts(t, p, q, 1, 2*time.Second)

	if p.states.len() != 1 {
		t.Fatalf("retry state must be RETAINED after a temporary failure (not deleted), got %d entries", p.states.len())
	}
	state := p.states.get("j1")
	if state.Count() != 1 {
		t.Fatalf("expected 1 recorded delivery operation after first claim, got %d", state.Count())
	}

	// Second claim cycle: must reuse the SAME state (same pool instance).
	q2 := mustQ(t, 4, queue.WithClock(func() time.Time { return sched1.NextRetryAt }))
	if err := q2.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1", AvailableAt: sched1.NextRetryAt}); err != nil {
		t.Fatal(err)
	}
	p.q = q2 // simulate the same pool continuing to run against fresh availability
	runPoolForAttempts(t, p, q2, 2, 2*time.Second)

	if c.totalAttempts() != 2 {
		t.Fatalf("expected exactly 2 attempts using persisted history, got %d", c.totalAttempts())
	}
	if p.states.len() != 0 {
		t.Fatalf("state must be forgotten after the job reaches a terminal outcome, got %d", p.states.len())
	}
}

// -------------------------------------------------------- missing msg --

func TestMissingMessageDoesNotAttemptSMTPAndReleases(t *testing.T) {
	q := mustQ(t, 4)
	l := newFakeLoader() // "m1" never populated
	c := newScriptedCoordinator()
	errFn, errs, mu := collectErrors()
	p := mustPool(t, q, l, c, Config{Workers: 1}, WithOnError(*errFn))

	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	select {
	case <-l.notify:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the loader to be called")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not shut down")
	}

	if c.totalAttempts() != 0 {
		t.Fatalf("missing message must never reach SMTP, attempts=%d", c.totalAttempts())
	}
	if q.Len() != 1 {
		t.Fatalf("missing-message job must be released, not lost, Len=%d", q.Len())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*errs) == 0 {
		t.Fatal("expected an operational error to be reported for the missing message")
	}
}

func TestMalformedEnvelopeReleasesWithOperationalError(t *testing.T) {
	q := mustQ(t, 4)
	l := newFakeLoader()
	l.put("m1", storage.StoredEnvelope{MailFrom: "<a@x>", RcptTo: nil}, "x\r\n") // no recipients
	c := newScriptedCoordinator()
	errFn, errs, mu := collectErrors()
	p := mustPool(t, q, l, c, Config{Workers: 1}, WithOnError(*errFn))

	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	runPoolUntilIdleOnce(t, p, q, 2*time.Second)

	if c.totalAttempts() != 0 {
		t.Fatalf("malformed envelope must never reach SMTP, attempts=%d", c.totalAttempts())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*errs) == 0 {
		t.Fatal("expected operational error for malformed envelope")
	}
}

// runPoolUntilIdleOnce is like runPoolUntilIdle but tolerates the job
// staying present (e.g. because it keeps getting released), stopping once
// at least one loader call has happened. Requires a *fakeLoader.
func runPoolUntilIdleOnce(t *testing.T, p *Pool, q *queue.MemoryQueue, timeout time.Duration) {
	t.Helper()
	l, ok := p.loader.(*fakeLoader)
	if !ok {
		t.Fatal("runPoolUntilIdleOnce requires a *fakeLoader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	select {
	case <-l.notify:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for the loader to be called")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not shut down")
	}
}

// ---------------------------------------------------------- panics -----

func TestPanicDuringProcessingIsRecoveredAndReleases(t *testing.T) {
	q := mustQ(t, 4)
	l := newFakeLoader()
	l.put("m1", envelopeFor("<panic@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator()
	c.script("<panic@x>", coordOutcome{panic: "boom"})
	errFn, errs, mu := collectErrors()
	p := mustPool(t, q, l, c, Config{Workers: 1}, WithOnError(*errFn))

	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	runPoolUntilIdleOnce(t, p, q, 2*time.Second)

	if q.Len() != 1 {
		t.Fatalf("panicking job must be released, not lost or stuck, Len=%d", q.Len())
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, e := range *errs {
		if strings.Contains(e.Error(), "recovered panic") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a recovered-panic operational error, got %v", *errs)
	}
}

// ------------------------------------------------------- attempt errors -

func TestAttemptErrorNotRetryableDefensivelyAcks(t *testing.T) {
	q := mustQ(t, 4)
	l := newFakeLoader()
	l.put("m1", envelopeFor("<bug@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator()
	c.script("<bug@x>", coordOutcome{err: retry.ErrNotRetryable})
	errFn, errs, mu := collectErrors()
	p := mustPool(t, q, l, c, Config{Workers: 1}, WithOnError(*errFn))

	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	runPoolUntilIdle(t, p, q, 2*time.Second)

	if q.Len() != 0 {
		t.Fatalf("ErrNotRetryable must be handled defensively via ack (stop the loop), Len=%d", q.Len())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*errs) == 0 {
		t.Fatal("expected an operational error reported")
	}
}
