package worker

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/retry"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

// newConcurrentLoader pre-populates n messages ("m0".."m(n-1)") whose
// MailFrom is "<m<i>@x>", letting the scriptedCoordinator key scripts per
// job without hand-registering every message individually.
func newConcurrentLoader(n int) *fakeLoader {
	l := newFakeLoader()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("m%d", i)
		l.put(id, envelopeFor(fmt.Sprintf("<%s@x>", id), "<b@y>"), "x\r\n")
	}
	return l
}

// TestMaxConcurrencyNeverExceedsConfiguredWorkers is the milestone's
// explicit requirement: instrument the coordinator to record concurrent
// in-flight Attempt calls and assert the observed maximum never exceeds
// Config.Workers, using many more jobs than workers and an artificial
// per-attempt delay so overlap is actually exercised.
func TestMaxConcurrencyNeverExceedsConfiguredWorkers(t *testing.T) {
	const workers = 4
	const jobs = 20
	q := mustQ(t, jobs)
	l := newConcurrentLoader(jobs)
	c := newScriptedCoordinator()
	c.delay = 15 * time.Millisecond
	for i := 0; i < jobs; i++ {
		key := fmt.Sprintf("<m%d@x>", i)
		c.script(key, coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true, Kind: delivery.KindAccepted}}})
	}
	for i := 0; i < jobs; i++ {
		id := fmt.Sprintf("m%d", i)
		if err := q.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id}); err != nil {
			t.Fatal(err)
		}
	}

	p := mustPool(t, q, l, c, Config{Workers: workers})
	runPoolForAttempts(t, p, q, jobs, 5*time.Second)

	if c.totalAttempts() != jobs {
		t.Fatalf("expected %d attempts, got %d", jobs, c.totalAttempts())
	}
	if c.maxConcurrent() > workers {
		t.Fatalf("observed max concurrency %d exceeds configured %d workers", c.maxConcurrent(), workers)
	}
	if c.maxConcurrent() < 2 {
		t.Fatalf("test did not actually exercise overlap (maxConcurrent=%d); delay/job count may need tuning", c.maxConcurrent())
	}
	if q.Len() != 0 {
		t.Fatalf("queue must drain fully, Len=%d", q.Len())
	}
}

func TestWorkerCountOne(t *testing.T) {
	const jobs = 5
	q := mustQ(t, jobs)
	l := newConcurrentLoader(jobs)
	c := newScriptedCoordinator()
	for i := 0; i < jobs; i++ {
		c.script(fmt.Sprintf("<m%d@x>", i), coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true}}})
	}
	for i := 0; i < jobs; i++ {
		id := fmt.Sprintf("m%d", i)
		_ = q.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id})
	}
	p := mustPool(t, q, l, c, Config{Workers: 1})
	runPoolForAttempts(t, p, q, jobs, 3*time.Second)
	if c.maxConcurrent() != 1 {
		t.Fatalf("single worker must never process concurrently, observed max=%d", c.maxConcurrent())
	}
}

func TestFewerJobsThanWorkers(t *testing.T) {
	const jobs = 2
	q := mustQ(t, jobs)
	l := newConcurrentLoader(jobs)
	c := newScriptedCoordinator()
	for i := 0; i < jobs; i++ {
		c.script(fmt.Sprintf("<m%d@x>", i), coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true}}})
	}
	for i := 0; i < jobs; i++ {
		id := fmt.Sprintf("m%d", i)
		_ = q.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id})
	}
	p := mustPool(t, q, l, c, Config{Workers: 8}) // more workers than jobs
	runPoolForAttempts(t, p, q, jobs, 3*time.Second)
	if c.totalAttempts() != jobs {
		t.Fatalf("expected %d attempts, got %d", jobs, c.totalAttempts())
	}
	if q.Len() != 0 {
		t.Fatalf("queue must drain, Len=%d", q.Len())
	}
}

func TestMixedSuccessFailureConcurrent(t *testing.T) {
	const jobs = 12
	q := mustQ(t, jobs)
	l := newConcurrentLoader(jobs)
	c := newScriptedCoordinator()
	for i := 0; i < jobs; i++ {
		key := fmt.Sprintf("<m%d@x>", i)
		if i%3 == 0 {
			c.script(key, coordOutcome{outcome: retry.Outcome{Status: retry.StatusFailed, Result: delivery.Result{Kind: delivery.KindTransferPermanent, FinalCode: 550}}})
		} else if i%3 == 1 {
			sched := retry.Schedule{Attempt: 1, Delay: time.Hour, NextRetryAt: time.Now().Add(time.Hour)}
			c.script(key, coordOutcome{outcome: retry.Outcome{Status: retry.StatusRetryable, Result: delivery.Result{Kind: delivery.KindTransferTemporary, FinalCode: 451}, Schedule: &sched}})
		} else {
			c.script(key, coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true}}})
		}
	}
	for i := 0; i < jobs; i++ {
		id := fmt.Sprintf("m%d", i)
		_ = q.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id})
	}
	p := mustPool(t, q, l, c, Config{Workers: 4})
	runPoolForAttempts(t, p, q, jobs, 3*time.Second)

	if c.totalAttempts() != jobs {
		t.Fatalf("expected %d attempts, got %d", jobs, c.totalAttempts())
	}
	// 1/3 succeeded, 1/3 permanently failed -> both acked; 1/3 temporary ->
	// released, still tracked.
	wantRemaining := 0
	for i := 0; i < jobs; i++ {
		if i%3 == 1 {
			wantRemaining++
		}
	}
	if q.Len() != wantRemaining {
		t.Fatalf("expected %d jobs remaining (released temporaries), got %d", wantRemaining, q.Len())
	}
}

func TestManyDelayedJobsDoNotBlockAvailableOnes(t *testing.T) {
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := mustQ(t, 20, queue.WithClock(func() time.Time { return frozen }))
	l := newConcurrentLoader(11)
	c := newScriptedCoordinator()

	// 10 far-future jobs, 1 immediately available.
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("m%d", i)
		if err := q.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id, AvailableAt: frozen.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	c.script("<m10@x>", coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true}}})
	if err := q.Enqueue(context.Background(), queue.Job{ID: "m10", MessageID: "m10"}); err != nil {
		t.Fatal(err)
	}

	p := mustPool(t, q, l, c, Config{Workers: 2}, WithClock(func() time.Time { return frozen }))
	runPoolForAttempts(t, p, q, 1, 2*time.Second)

	if c.totalAttempts() != 1 {
		t.Fatalf("expected exactly 1 attempt (the available job), got %d", c.totalAttempts())
	}
	if q.Len() != 10 {
		t.Fatalf("delayed jobs must remain untouched, Len=%d", q.Len())
	}
}

// -------------------------------------------------------- backpressure --

func TestQueueBackpressureNotBypassed(t *testing.T) {
	// Capacity smaller than job count: producer blocks; worker draining
	// must be the only thing that frees capacity — no internal unbounded
	// buffering inside the pool.
	const capacity = 3
	const jobs = 15
	q := mustQ(t, capacity)
	l := newConcurrentLoader(jobs)
	c := newScriptedCoordinator()
	for i := 0; i < jobs; i++ {
		c.script(fmt.Sprintf("<m%d@x>", i), coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true}}})
	}
	p := mustPool(t, q, l, c, Config{Workers: 2})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	var wg sync.WaitGroup
	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("m%d", i)
			if err := q.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id}); err != nil {
				t.Errorf("enqueue %d: %v", i, err)
			}
		}(i)
	}
	// Producers must complete (queue capacity + worker draining eventually
	// admits all of them); this alone proves backpressure did not
	// deadlock and the pool did drain concurrently with production.
	waitDone := make(chan struct{})
	go func() { wg.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("producers never completed — capacity/backpressure likely deadlocked")
	}

	c.waitAttempts(t, jobs, 5*time.Second)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not shut down")
	}
	if c.totalAttempts() != jobs {
		t.Fatalf("expected %d attempts, got %d", jobs, c.totalAttempts())
	}
}

// ------------------------------------------------------------ shutdown --

// TestShutdownWithInFlightAttemptCompletesAckCleanly proves cancellation
// during an in-flight Attempt call still lets that job's Ack/Release
// complete (Run does not return until the CURRENT job's bookkeeping is
// done), using a barrier so the test controls exactly when the in-flight
// call unblocks relative to cancellation — no sleep.
func TestShutdownWithInFlightAttemptCompletesAckCleanly(t *testing.T) {
	q := mustQ(t, 4)
	l := newFakeLoader()
	l.put("m1", envelopeFor("<a@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator()
	c.barrier = make(chan struct{})
	c.entered = make(chan struct{}, 1)
	c.script("<a@x>", coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true}}})

	p := mustPool(t, q, l, c, Config{Workers: 1})
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	// Wait until Attempt itself is in flight. Waiting only for Load was a
	// race: cancel() could land before Attempt started, and the real
	// coordinator (and this fake) then short-circuit on the canceled
	// context, releasing the job instead of acking it.
	select {
	case <-c.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached Attempt")
	}
	cancel() // shutdown requested WHILE Attempt is blocked on the barrier

	// The barrier release simulates "SMTP finally returns" after shutdown
	// was already requested.
	close(c.barrier)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not shut down after in-flight attempt completed")
	}
	if q.Len() != 0 {
		t.Fatalf("accepted delivery must still be acked even though shutdown began first, Len=%d", q.Len())
	}
}

// TestShutdownReleasesClaimNeverAbandoned proves that when shutdown is
// requested before a claimed job ever reaches Attempt (canceled while
// Load is in flight is approximated here by canceling immediately after
// Claim, before Load starts, via a loader that blocks on a barrier), the
// job is Released rather than lost.
func TestShutdownReleasesClaimBeforeAttempt(t *testing.T) {
	q := mustQ(t, 4)
	l := newBarrierLoader()
	l.put("m1", envelopeFor("<a@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator()
	c.script("<a@x>", coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true}}})

	p := mustPool(t, q, l, c, Config{Workers: 1})
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	select {
	case <-l.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never reached Load")
	}
	cancel()
	close(l.barrier) // let Load proceed (now that ctx is already canceled)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not shut down")
	}
	if q.Len() != 1 {
		t.Fatalf("claim taken before shutdown must be released, not abandoned or lost, Len=%d", q.Len())
	}
	if c.totalAttempts() != 0 {
		t.Fatalf("shutdown before Attempt must never reach SMTP, attempts=%d", c.totalAttempts())
	}
}

// barrierLoader wraps fakeLoader, blocking each Load call on a barrier
// channel and signaling entered exactly once per call, so a test can
// deterministically synchronize "worker is inside Load" without sleep.
type barrierLoader struct {
	*fakeLoader
	barrier chan struct{}
	entered chan struct{}
}

func newBarrierLoader() *barrierLoader {
	return &barrierLoader{fakeLoader: newFakeLoader(), barrier: make(chan struct{}), entered: make(chan struct{}, 8)}
}

func (b *barrierLoader) Load(id string) (storage.StoredMessage, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.barrier
	return b.fakeLoader.Load(id)
}

func TestCloseQueueAtStartupCausesCleanExit(t *testing.T) {
	q := mustQ(t, 1)
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	l := newFakeLoader()
	c := newScriptedCoordinator()
	p := mustPool(t, q, l, c, Config{Workers: 3})

	done := make(chan struct{})
	go func() { p.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool with an already-closed queue must exit promptly")
	}
}

func TestCancelBeforeRun(t *testing.T) {
	q := mustQ(t, 1)
	l := newFakeLoader()
	c := newScriptedCoordinator()
	p := mustPool(t, q, l, c, Config{Workers: 3})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run with an already-canceled context must exit promptly")
	}
}

func TestQueueClosesWhileRunning(t *testing.T) {
	const jobs = 6
	q := mustQ(t, jobs)
	l := newConcurrentLoader(jobs)
	c := newScriptedCoordinator()
	for i := 0; i < jobs; i++ {
		c.script(fmt.Sprintf("<m%d@x>", i), coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true}}})
	}
	for i := 0; i < jobs; i++ {
		id := fmt.Sprintf("m%d", i)
		_ = q.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id})
	}
	p := mustPool(t, q, l, c, Config{Workers: 3})

	ctx := context.Background()
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	c.waitAttempts(t, jobs, 3*time.Second)
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not exit after queue closed")
	}
}

// TestRepeatedRunAfterClose ensures a second Run call on an already-closed
// queue also exits cleanly (Pool itself has no Start/Stop state machine to
// corrupt, but this exercises the same Pool value being reused).
func TestRepeatedRunAfterClose(t *testing.T) {
	q := mustQ(t, 1)
	l := newFakeLoader()
	c := newScriptedCoordinator()
	p := mustPool(t, q, l, c, Config{Workers: 2})
	_ = q.Close()

	for i := 0; i < 2; i++ {
		done := make(chan struct{})
		go func() { p.Run(context.Background()); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("run %d did not exit", i)
		}
	}
}

// ------------------------------------------------------- goroutine leak --

func TestNoGoroutineLeakAfterShutdown(t *testing.T) {
	before := runtime.NumGoroutine()
	const jobs = 10
	q := mustQ(t, jobs)
	l := newConcurrentLoader(jobs)
	c := newScriptedCoordinator()
	for i := 0; i < jobs; i++ {
		c.script(fmt.Sprintf("<m%d@x>", i), coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true}}})
	}
	for i := 0; i < jobs; i++ {
		id := fmt.Sprintf("m%d", i)
		_ = q.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id})
	}
	p := mustPool(t, q, l, c, Config{Workers: 5})
	runPoolForAttempts(t, p, q, jobs, 3*time.Second)

	after := waitForGoroutineCount(t, before, 2*time.Second)
	if after > before {
		t.Fatalf("goroutine count grew from %d to %d after shutdown", before, after)
	}
}

func waitForGoroutineCount(t *testing.T, baseline int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		last = runtime.NumGoroutine()
		if last <= baseline {
			return last
		}
		// Bounded, short poll for goroutine scheduler settling only — not
		// used to coordinate test correctness, only to avoid a false
		// positive from goroutines that haven't finished unwinding yet.
		time.Sleep(5 * time.Millisecond)
	}
	return last
}
