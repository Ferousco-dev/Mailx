package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/retry"
	goredis "github.com/redis/go-redis/v9"
)

// v0.16 worker/RedisQueue integration: proves the unmodified v0.14
// worker.Pool drives correctly off a distributed RedisQueue, including
// the scenario MemoryQueue could never exercise — a worker that dies
// mid-claim and has its job reclaimed by another pool entirely.

const redisIntegrationAddr = "localhost:6379"

func requireRedisIntegration(t *testing.T) {
	t.Helper()
	client := goredis.NewClient(&goredis.Options{Addr: redisIntegrationAddr})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("worker/RedisQueue integration requires real Redis at %s: %v", redisIntegrationAddr, err)
	}
}

func mustRedisQ(t *testing.T, namespace string, lease time.Duration) *queue.RedisQueue {
	t.Helper()
	q, err := queue.NewRedisQueue(queue.RedisConfig{
		Addr: redisIntegrationAddr, Namespace: namespace, Capacity: 64,
		ClaimLease: lease, PollInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func randNamespace(t *testing.T) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "wker-" + hex.EncodeToString(b)
}

// F: multiple workers within ONE pool sharing one RedisQueue each claim a
// distinct job — proves worker.Pool's existing goroutine-per-worker model
// composes with distributed claim atomicity.
func TestRedisIntegrationMultipleWorkersOnePool(t *testing.T) {
	requireRedisIntegration(t)
	q := mustRedisQ(t, randNamespace(t), time.Minute)

	l := newFakeLoader()
	c := newScriptedCoordinator()
	c.delay = 100 * time.Millisecond // forces overlap so concurrency is observable
	const jobs = 20
	for i := 0; i < jobs; i++ {
		id := "job" + string(rune('a'+i))
		l.put(id, envelopeFor("<a@x>", "<b@y>"), "Subject: x\r\n\r\nbody\r\n")
		c.script("<a@x>", coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true, Kind: delivery.KindAccepted}}})
	}
	// scriptedCoordinator scripts are keyed by MailFrom, shared across
	// jobs here, so give it enough scripted successes for every job.
	for i := 1; i < jobs; i++ {
		c.script("<a@x>", coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true, Kind: delivery.KindAccepted}}})
	}

	p := mustPool(t, q, l, c, Config{Workers: 5})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	for i := 0; i < jobs; i++ {
		id := "job" + string(rune('a'+i))
		if err := q.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id}); err != nil {
			t.Fatal(err)
		}
	}

	c.waitAttempts(t, jobs, 5*time.Second)
	cancel()
	<-done

	if got := c.maxConcurrent(); got < 2 {
		t.Fatalf("expected real worker concurrency against RedisQueue, max concurrent was %d", got)
	}
}

// G: two independent worker pools, each with its OWN RedisQueue client,
// share one Redis namespace — simulating two MailX processes.
func TestRedisIntegrationTwoIndependentPoolsShareNamespace(t *testing.T) {
	requireRedisIntegration(t)
	ns := randNamespace(t)
	qA := mustRedisQ(t, ns, time.Minute)
	qB := mustRedisQ(t, ns, time.Minute)

	l := newFakeLoader()
	c := newScriptedCoordinator()
	const jobs = 30
	for i := 0; i < jobs; i++ {
		id := "shared" + string(rune('a'+i))
		l.put(id, envelopeFor("<a@x>", "<b@y>"), "Subject: x\r\n\r\nbody\r\n")
		c.script("<a@x>", coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true, Kind: delivery.KindAccepted}}})
	}

	pA := mustPool(t, qA, l, c, Config{Workers: 2})
	pB := mustPool(t, qB, l, c, Config{Workers: 2})
	ctx, cancel := context.WithCancel(context.Background())
	doneA, doneB := make(chan struct{}), make(chan struct{})
	go func() { pA.Run(ctx); close(doneA) }()
	go func() { pB.Run(ctx); close(doneB) }()

	for i := 0; i < jobs; i++ {
		id := "shared" + string(rune('a'+i))
		if err := qA.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id}); err != nil {
			t.Fatal(err)
		}
	}

	c.waitAttempts(t, jobs, 5*time.Second)
	cancel()
	<-doneA
	<-doneB
	// Every job completed exactly once across both independent processes
	// is implied by c.waitAttempts(jobs) succeeding: a double-claim would
	// either double the attempt count (caught by RedisQueue's own
	// TestMultipleClientsExactlyOneOwnerPerClaim) or hang.
}

// H: a worker "process" dies holding a claim (its Pool is torn down
// without Ack/Release); once the lease expires, a second, independent
// pool on the same namespace reclaims and completes the job.
func TestRedisIntegrationWorkerCrashClaimRecovery(t *testing.T) {
	requireRedisIntegration(t)
	ns := randNamespace(t)
	const lease = 150 * time.Millisecond
	qA := mustRedisQ(t, ns, lease)

	l := newFakeLoader()
	l.put("crashjob", envelopeFor("<a@x>", "<b@y>"), "Subject: x\r\n\r\nbody\r\n")

	// Pool A's coordinator blocks forever (simulating a hung/dead worker):
	// it claims the job but never completes the Attempt, so it never
	// Acks or Releases.
	blockingCoord := newScriptedCoordinator()
	blockingCoord.delay = 10 * time.Second
	pA := mustPool(t, qA, l, blockingCoord, Config{Workers: 1})
	ctxA, cancelA := context.WithCancel(context.Background())
	doneA := make(chan struct{})
	go func() { pA.Run(ctxA); close(doneA) }()

	if err := qA.Enqueue(context.Background(), queue.Job{ID: "crashjob", MessageID: "crashjob"}); err != nil {
		t.Fatal(err)
	}
	// Give A time to claim, then kill it (cancel + close its client)
	// without ever letting Attempt return.
	time.Sleep(50 * time.Millisecond)
	cancelA()
	_ = qA.Close()
	<-doneA

	// Pool B: independent client, same namespace, healthy coordinator.
	qB := mustRedisQ(t, ns, lease)
	realCoord := newScriptedCoordinator()
	realCoord.script("<a@x>", coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true, Kind: delivery.KindAccepted}}})
	pB := mustPool(t, qB, l, realCoord, Config{Workers: 1})
	ctxB, cancelB := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelB()
	doneB := make(chan struct{})
	go func() { pB.Run(ctxB); close(doneB) }()

	realCoord.waitAttempts(t, 1, 3*time.Second)
	cancelB()
	<-doneB
}
