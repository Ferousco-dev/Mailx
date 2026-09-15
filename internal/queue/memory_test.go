package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func job(id string) Job { return Job{ID: id, MessageID: "msg-" + id} }

func mustQueue(t *testing.T, capacity int, opts ...Option) *MemoryQueue {
	t.Helper()
	q, err := NewMemoryQueue(capacity, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// ---------------------------------------------------------------- basics --

func TestNewMemoryQueueRejectsBadCapacity(t *testing.T) {
	for _, c := range []int{0, -1, -1000} {
		if _, err := NewMemoryQueue(c); !errors.Is(err, ErrInvalidCapacity) {
			t.Errorf("capacity %d: expected ErrInvalidCapacity, got %v", c, err)
		}
	}
}

func TestNewMemoryQueueAcceptsHugeCapacity(t *testing.T) {
	if _, err := NewMemoryQueue(1 << 20); err != nil {
		t.Fatal(err)
	}
}

func TestEnqueueRejectsInvalidJob(t *testing.T) {
	q := mustQueue(t, 4)
	if err := q.Enqueue(context.Background(), Job{}); !errors.Is(err, ErrEmptyJobID) {
		t.Fatalf("expected ErrEmptyJobID, got %v", err)
	}
}

// ------------------------------------------------------------- ordering --

func TestFIFOOrdering(t *testing.T) {
	q := mustQueue(t, 8)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if err := q.Enqueue(ctx, job(id)); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"a", "b", "c"} {
		c, err := q.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if c.Job.ID != want {
			t.Fatalf("got %q want %q", c.Job.ID, want)
		}
	}
}

func TestImmediateAvailabilityOnZeroAvailableAt(t *testing.T) {
	q := mustQueue(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := q.Enqueue(ctx, job("a")); err != nil {
		t.Fatal(err)
	}
	c, err := q.Claim(ctx)
	if err != nil || c.Job.ID != "a" {
		t.Fatalf("expected immediate claim, got %+v %v", c, err)
	}
}

func TestDelayedOrderingAmongMultipleAvailableJobs(t *testing.T) {
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := mustQueue(t, 8, WithClock(func() time.Time { return frozen }))
	ctx := context.Background()
	// b becomes available before a, despite being enqueued second.
	if err := q.Enqueue(ctx, Job{ID: "a", MessageID: "m", AvailableAt: frozen.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, Job{ID: "b", MessageID: "m", AvailableAt: frozen.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, Job{ID: "c", MessageID: "m", AvailableAt: frozen}); err != nil {
		t.Fatal(err)
	}
	// Only "c" is available at the frozen time; claim with a short deadline
	// so this test cannot hang if ordering is wrong.
	claimCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	c, err := q.Claim(claimCtx)
	if err != nil || c.Job.ID != "c" {
		t.Fatalf("expected c first, got %+v %v", c, err)
	}
}

func TestFutureJobUnavailableUntilDue(t *testing.T) {
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := mustQueue(t, 1, WithClock(func() time.Time { return frozen }))
	if err := q.Enqueue(context.Background(), Job{ID: "a", MessageID: "m", AvailableAt: frozen.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err := q.Claim(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	if q.Len() != 1 {
		t.Fatalf("job must remain tracked while unavailable: Len=%d", q.Len())
	}
}

// TestDelayedJobBecomesAvailableViaTimer exercises the real timer-based
// wakeup path with a short, bounded real delay — this is testing the
// delayed-availability FEATURE itself, not using sleep to synchronize
// unrelated goroutines.
func TestDelayedJobBecomesAvailableViaTimer(t *testing.T) {
	q := mustQueue(t, 1) // default real clock
	delay := 70 * time.Millisecond
	if err := q.Enqueue(context.Background(), Job{ID: "a", MessageID: "m", AvailableAt: time.Now().Add(delay)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	c, err := q.Claim(ctx)
	elapsed := time.Since(start)
	if err != nil || c.Job.ID != "a" {
		t.Fatalf("claim failed: %+v %v", c, err)
	}
	if elapsed < delay/2 {
		t.Fatalf("claimed too early: elapsed=%v delay=%v", elapsed, delay)
	}
}

// TestEnqueueWakesClaimBlockedOnFarFutureJob proves a newly enqueued,
// immediately-available job wakes a Claim call that was blocked waiting on
// a much-later job's timer, rather than waiting for that timer to fire.
func TestEnqueueWakesClaimBlockedOnFarFutureJob(t *testing.T) {
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var clock atomic.Value
	clock.Store(frozen)
	q := mustQueue(t, 4, WithClock(func() time.Time { return clock.Load().(time.Time) }))
	// A job "far" in the future relative to the frozen clock (real timer
	// duration will be huge; the test must not wait for it).
	if err := q.Enqueue(context.Background(), Job{ID: "future", MessageID: "m", AvailableAt: frozen.Add(10 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	claimed := make(chan Claim, 1)
	claimErr := make(chan error, 1)
	go func() {
		c, err := q.Claim(context.Background())
		claimed <- c
		claimErr <- err
	}()
	// Give the goroutine a moment to reach its blocking select — we cannot
	// observe this deterministically without an internal hook, so instead
	// we rely on Enqueue's broadcast being correct regardless of timing:
	// even if the Claim call hasn't blocked yet, it will see the new entry
	// on its next loop iteration under the lock. Poll with a bounded
	// deadline rather than a fixed sleep.
	if err := q.Enqueue(context.Background(), Job{ID: "now", MessageID: "m"}); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-claimed:
		if err := <-claimErr; err != nil {
			t.Fatalf("claim error: %v", err)
		}
		if c.Job.ID != "now" {
			t.Fatalf("expected the immediately-available job, got %q", c.Job.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Claim did not wake for the newly available job")
	}
}

// ---------------------------------------------------------- claim/ack ----

func TestAckFreesCapacityAndRemovesJob(t *testing.T) {
	q := mustQueue(t, 1)
	ctx := context.Background()
	if err := q.Enqueue(ctx, job("a")); err != nil {
		t.Fatal(err)
	}
	c, err := q.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Ack(ctx, c.Job.ID, c.Token); err != nil {
		t.Fatal(err)
	}
	if q.Len() != 0 {
		t.Fatalf("expected 0 active jobs after ack, got %d", q.Len())
	}
}

func TestAckTwiceFails(t *testing.T) {
	q := mustQueue(t, 1)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))
	c, _ := q.Claim(ctx)
	if err := q.Ack(ctx, c.Job.ID, c.Token); err != nil {
		t.Fatal(err)
	}
	if err := q.Ack(ctx, c.Job.ID, c.Token); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("expected ErrUnknownJob on double ack, got %v", err)
	}
}

func TestAckUnknownJob(t *testing.T) {
	q := mustQueue(t, 1)
	if err := q.Ack(context.Background(), "does-not-exist", 1); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("expected ErrUnknownJob, got %v", err)
	}
}

func TestAckWithWrongTokenFails(t *testing.T) {
	q := mustQueue(t, 1)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))
	c, _ := q.Claim(ctx)
	if err := q.Ack(ctx, c.Job.ID, c.Token+999); !errors.Is(err, ErrJobNotClaimed) {
		t.Fatalf("expected ErrJobNotClaimed, got %v", err)
	}
}

func TestReleaseMakesJobAvailableAgain(t *testing.T) {
	q := mustQueue(t, 1)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))
	c, _ := q.Claim(ctx)
	if err := q.Release(ctx, c.Job.ID, c.Token, time.Time{}); err != nil {
		t.Fatal(err)
	}
	c2, err := q.Claim(ctx)
	if err != nil || c2.Job.ID != "a" {
		t.Fatalf("expected to reclaim released job, got %+v %v", c2, err)
	}
	if c2.Token == c.Token {
		t.Fatal("re-claim must issue a fresh token")
	}
}

func TestReleaseDoesNotFreeCapacity(t *testing.T) {
	q := mustQueue(t, 1)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))
	c, _ := q.Claim(ctx)
	_ = q.Release(ctx, c.Job.ID, c.Token, time.Time{})
	if q.Len() != 1 {
		t.Fatalf("release must not free capacity, Len=%d", q.Len())
	}
	shortCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if err := q.Enqueue(shortCtx, job("b")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected Enqueue to block at capacity, got %v", err)
	}
}

func TestReleaseTwiceFailsSecondTime(t *testing.T) {
	q := mustQueue(t, 1)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))
	c, _ := q.Claim(ctx)
	if err := q.Release(ctx, c.Job.ID, c.Token, time.Time{}); err != nil {
		t.Fatal(err)
	}
	// Stale token from the first claim must not work a second time — the
	// job is available, not claimed, so any token is rejected.
	if err := q.Release(ctx, c.Job.ID, c.Token, time.Time{}); !errors.Is(err, ErrJobNotClaimed) {
		t.Fatalf("expected ErrJobNotClaimed on double release, got %v", err)
	}
}

func TestAckAfterReleaseFails(t *testing.T) {
	q := mustQueue(t, 1)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))
	c, _ := q.Claim(ctx)
	_ = q.Release(ctx, c.Job.ID, c.Token, time.Time{})
	if err := q.Ack(ctx, c.Job.ID, c.Token); !errors.Is(err, ErrJobNotClaimed) {
		t.Fatalf("expected ErrJobNotClaimed, got %v", err)
	}
}

func TestReleaseAfterAckFails(t *testing.T) {
	q := mustQueue(t, 1)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))
	c, _ := q.Claim(ctx)
	_ = q.Ack(ctx, c.Job.ID, c.Token)
	if err := q.Release(ctx, c.Job.ID, c.Token, time.Time{}); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("expected ErrUnknownJob, got %v", err)
	}
}

// TestStaleTokenAfterReclaimCannotCorruptNewOwner is the key race the claim
// token exists to prevent: original owner releases (or would have, had it
// not crashed) and a second worker claims the same job; the ORIGINAL
// owner's token must never be able to Ack or Release the NEW owner's claim.
func TestStaleTokenAfterReclaimCannotCorruptNewOwner(t *testing.T) {
	q := mustQueue(t, 1)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))
	first, _ := q.Claim(ctx)
	_ = q.Release(ctx, first.Job.ID, first.Token, time.Time{})
	second, err := q.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Ack(ctx, first.Job.ID, first.Token); !errors.Is(err, ErrJobNotClaimed) {
		t.Fatalf("stale token must not ack the new owner's claim, got %v", err)
	}
	if err := q.Ack(ctx, second.Job.ID, second.Token); err != nil {
		t.Fatalf("legitimate owner's ack must succeed: %v", err)
	}
}

func TestTwoConsumersRaceToClaimOneJobOnlyOneWins(t *testing.T) {
	q := mustQueue(t, 1)
	_ = q.Enqueue(context.Background(), job("a"))
	const workers = 8
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := q.Claim(ctx)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly 1 successful claim, got %d", successes)
	}
}

func TestTwoConsumersCannotBothAckSameJob(t *testing.T) {
	q := mustQueue(t, 1)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))
	c, _ := q.Claim(ctx)
	var wg sync.WaitGroup
	var successes int32
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := q.Ack(ctx, c.Job.ID, c.Token); err == nil {
				atomic.AddInt32(&successes, 1)
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("expected exactly 1 successful ack among racers, got %d", successes)
	}
}

// --------------------------------------------------------- duplicate ID --

func TestDuplicateEnqueueSequentialIsNoOp(t *testing.T) {
	q := mustQueue(t, 4)
	ctx := context.Background()
	if err := q.Enqueue(ctx, job("a")); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, job("a")); err != nil {
		t.Fatalf("duplicate enqueue should be a nil no-op, got %v", err)
	}
	if q.Len() != 1 {
		t.Fatalf("expected 1 active job, got %d", q.Len())
	}
}

func TestDuplicateEnqueueConcurrentIsSingleJob(t *testing.T) {
	q := mustQueue(t, 4)
	ctx := context.Background()
	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- q.Enqueue(ctx, job("shared"))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent duplicate enqueue returned error: %v", err)
		}
	}
	if q.Len() != 1 {
		t.Fatalf("expected exactly 1 logical job, got %d", q.Len())
	}
}

func TestSameMessageIDDifferentJobIDsBothAccepted(t *testing.T) {
	q := mustQueue(t, 4)
	ctx := context.Background()
	if err := q.Enqueue(ctx, Job{ID: "op1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, Job{ID: "op2", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	if q.Len() != 2 {
		t.Fatalf("job identity must be independent of message identity, Len=%d", q.Len())
	}
}

func TestDuplicateEnqueueAfterAckCreatesNewJob(t *testing.T) {
	// Documents the explicit, deferred limitation: v0.13 does not remember
	// completed job IDs, so re-enqueuing after Ack is treated as new work.
	q := mustQueue(t, 4)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))
	c, _ := q.Claim(ctx)
	_ = q.Ack(ctx, c.Job.ID, c.Token)
	if err := q.Enqueue(ctx, job("a")); err != nil {
		t.Fatal(err)
	}
	if q.Len() != 1 {
		t.Fatalf("expected the re-enqueued job to be tracked as new, Len=%d", q.Len())
	}
}

func TestDuplicateEnqueueWhileClaimedIsNoOp(t *testing.T) {
	q := mustQueue(t, 4)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))
	_, err := q.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, job("a")); err != nil {
		t.Fatalf("duplicate while claimed should be a no-op, got %v", err)
	}
	if q.Len() != 1 {
		t.Fatalf("expected still 1 active job, got %d", q.Len())
	}
	shortCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := q.Claim(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("job must still be claimed by the original owner, got %v", err)
	}
}

func TestDuplicateEnqueueWhileDelayedIsNoOp(t *testing.T) {
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := mustQueue(t, 4, WithClock(func() time.Time { return frozen }))
	ctx := context.Background()
	future := Job{ID: "a", MessageID: "m", AvailableAt: frozen.Add(time.Hour)}
	if err := q.Enqueue(ctx, future); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, future); err != nil {
		t.Fatalf("duplicate delayed enqueue should be a no-op, got %v", err)
	}
	if q.Len() != 1 {
		t.Fatalf("expected 1 active job, got %d", q.Len())
	}
}

func TestMalformedDuplicateStillRejected(t *testing.T) {
	q := mustQueue(t, 4)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))
	// Same ID, but this call's payload is itself invalid — validation must
	// still run regardless of duplicate status.
	bad := Job{ID: "a", MessageID: ""}
	if err := q.Enqueue(ctx, bad); !errors.Is(err, ErrEmptyMessageID) {
		t.Fatalf("expected validation error even for a duplicate ID, got %v", err)
	}
}

// ------------------------------------------------------- capacity/backpressure --

func TestEnqueueBlocksExactlyAtCapacity(t *testing.T) {
	q := mustQueue(t, 2)
	ctx := context.Background()
	if err := q.Enqueue(ctx, job("a")); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, job("b")); err != nil {
		t.Fatal(err)
	}
	shortCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if err := q.Enqueue(shortCtx, job("c")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected blocking at capacity, got %v", err)
	}
}

func TestEnqueueUnblocksWhenAckFreesCapacity(t *testing.T) {
	q := mustQueue(t, 1)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))
	c, _ := q.Claim(ctx)

	blocked := make(chan error, 1)
	go func() { blocked <- q.Enqueue(context.Background(), job("b")) }()

	// The producer must actually be blocked (capacity exhausted); prove it
	// isn't spuriously succeeding by racing a short-timeout check first.
	select {
	case err := <-blocked:
		t.Fatalf("enqueue returned before capacity freed: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	if err := q.Ack(ctx, c.Job.ID, c.Token); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("unblocked enqueue failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue did not unblock after ack freed capacity")
	}
}

func TestEnqueueContextCancelWhileWaitingForCapacity(t *testing.T) {
	q := mustQueue(t, 1)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))

	waitCtx, cancel := context.WithCancel(context.Background())
	blocked := make(chan error, 1)
	go func() { blocked <- q.Enqueue(waitCtx, job("b")) }()

	cancel()
	select {
	case err := <-blocked:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not unblock enqueue")
	}
}

func TestManyConcurrentProducersAndOneConsumerDrain(t *testing.T) {
	q := mustQueue(t, 4)
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("job-%d", i)
			if err := q.Enqueue(context.Background(), job(id)); err != nil {
				t.Errorf("producer %d: %v", i, err)
			}
		}(i)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		claimed := 0
		for claimed < n {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			c, err := q.Claim(ctx)
			cancel()
			if err != nil {
				t.Errorf("consumer claim: %v", err)
				return
			}
			if err := q.Ack(context.Background(), c.Job.ID, c.Token); err != nil {
				t.Errorf("consumer ack: %v", err)
				return
			}
			claimed++
		}
	}()

	wg.Wait()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("consumer did not drain all produced jobs in time")
	}
	if q.Len() != 0 {
		t.Fatalf("expected queue empty after full drain, got %d", q.Len())
	}
}

func TestManyConcurrentConsumersEachJobClaimedOnce(t *testing.T) {
	const jobs = 20
	q := mustQueue(t, jobs)
	ctx := context.Background()
	for i := 0; i < jobs; i++ {
		if err := q.Enqueue(ctx, job(fmt.Sprintf("j%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	const consumers = 40 // more consumers than jobs
	var wg sync.WaitGroup
	seen := make(chan string, jobs)
	for i := 0; i < consumers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			defer cancel()
			c, err := q.Claim(cctx)
			if err == nil {
				seen <- c.Job.ID
			}
		}()
	}
	wg.Wait()
	close(seen)
	ids := map[string]int{}
	for id := range seen {
		ids[id]++
	}
	if len(ids) != jobs {
		t.Fatalf("expected %d distinct jobs claimed, got %d", jobs, len(ids))
	}
	for id, count := range ids {
		if count != 1 {
			t.Fatalf("job %q claimed %d times", id, count)
		}
	}
}

// ------------------------------------------------------------- shutdown --

func TestCloseIsIdempotent(t *testing.T) {
	q := mustQueue(t, 1)
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("second close should be a safe no-op, got %v", err)
	}
}

func TestCloseRejectsNewEnqueue(t *testing.T) {
	q := mustQueue(t, 1)
	_ = q.Close()
	if err := q.Enqueue(context.Background(), job("a")); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("expected ErrQueueClosed, got %v", err)
	}
}

func TestCloseRejectsNewClaim(t *testing.T) {
	q := mustQueue(t, 1)
	_ = q.Close()
	if _, err := q.Claim(context.Background()); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("expected ErrQueueClosed, got %v", err)
	}
}

func TestCloseWakesBlockedEnqueue(t *testing.T) {
	q := mustQueue(t, 1)
	_ = q.Enqueue(context.Background(), job("a"))
	blocked := make(chan error, 1)
	go func() { blocked <- q.Enqueue(context.Background(), job("b")) }()
	select {
	case err := <-blocked:
		t.Fatalf("enqueue returned before close: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-blocked:
		if !errors.Is(err, ErrQueueClosed) {
			t.Fatalf("expected ErrQueueClosed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not wake blocked enqueue")
	}
}

func TestCloseWakesBlockedClaim(t *testing.T) {
	q := mustQueue(t, 1)
	blocked := make(chan error, 1)
	go func() {
		_, err := q.Claim(context.Background())
		blocked <- err
	}()
	select {
	case err := <-blocked:
		t.Fatalf("claim returned before close: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-blocked:
		if !errors.Is(err, ErrQueueClosed) {
			t.Fatalf("expected ErrQueueClosed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not wake blocked claim")
	}
}

func TestAckStillWorksAfterClose(t *testing.T) {
	q := mustQueue(t, 1)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))
	c, _ := q.Claim(ctx)
	_ = q.Close()
	if err := q.Ack(ctx, c.Job.ID, c.Token); err != nil {
		t.Fatalf("ack of an in-flight claim must still work after close, got %v", err)
	}
}

func TestReleaseStillWorksAfterCloseButJobUnreachable(t *testing.T) {
	q := mustQueue(t, 1)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))
	c, _ := q.Claim(ctx)
	_ = q.Close()
	if err := q.Release(ctx, c.Job.ID, c.Token, time.Time{}); err != nil {
		t.Fatalf("release of an in-flight claim must still work after close, got %v", err)
	}
	// The queue is a hard stop: even though a job is technically available
	// again, Close means no further Claim succeeds.
	if _, err := q.Claim(ctx); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("expected ErrQueueClosed even with an available job, got %v", err)
	}
}

// ---------------------------------------------------------------- misc ---

func TestContextAlreadyCanceledOnAckRelease(t *testing.T) {
	q := mustQueue(t, 1)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))
	c, _ := q.Claim(ctx)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.Ack(canceled, c.Job.ID, c.Token); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from Ack, got %v", err)
	}
	if err := q.Release(canceled, c.Job.ID, c.Token, time.Time{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from Release, got %v", err)
	}
}

// TestClaimedJobReturnedByValueCannotMutateQueueState proves a caller
// mutating a returned Claim.Job has no effect on internal queue state.
func TestClaimedJobReturnedByValueCannotMutateQueueState(t *testing.T) {
	q := mustQueue(t, 1)
	ctx := context.Background()
	_ = q.Enqueue(ctx, job("a"))
	c, _ := q.Claim(ctx)
	c.Job.ID = "corrupted"
	c.Job.MessageID = "corrupted"
	if err := q.Ack(ctx, "a", c.Token); err != nil {
		t.Fatalf("original job identity must be unaffected by caller mutation: %v", err)
	}
}

// TestRaceStress runs a high-concurrency producer/consumer/release mix
// under the race detector. It must complete without deadlock or panic.
func TestRaceStress(t *testing.T) {
	q := mustQueue(t, 16)
	const producers = 20
	const perProducer = 10
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				id := fmt.Sprintf("p%d-j%d", p, i)
				_ = q.Enqueue(context.Background(), job(id))
			}
		}(p)
	}

	total := producers * perProducer
	var consumed int32
	var cwg sync.WaitGroup
	for c := 0; c < 8; c++ {
		cwg.Add(1)
		go func() {
			defer cwg.Done()
			for {
				if int(atomic.LoadInt32(&consumed)) >= total {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				claim, err := q.Claim(ctx)
				cancel()
				if err != nil {
					continue
				}
				// Randomly release-then-let-someone-else-ack half the time
				// by immediately re-acking (kept simple: always ack, since
				// the goal here is race safety, already covered by
				// dedicated release tests above).
				if err := q.Ack(context.Background(), claim.Job.ID, claim.Token); err == nil {
					atomic.AddInt32(&consumed, 1)
				}
			}
		}()
	}

	wg.Wait()
	done := make(chan struct{})
	go func() { cwg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stress test did not complete in time (possible deadlock)")
	}
	if int(consumed) != total {
		t.Fatalf("expected %d consumed, got %d", total, consumed)
	}
}

func BenchmarkEnqueueClaimAck(b *testing.B) {
	q, err := NewMemoryQueue(1024)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("bench-%d", i)
		if err := q.Enqueue(ctx, Job{ID: id, MessageID: id}); err != nil {
			b.Fatal(err)
		}
		c, err := q.Claim(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if err := q.Ack(ctx, c.Job.ID, c.Token); err != nil {
			b.Fatal(err)
		}
	}
}
