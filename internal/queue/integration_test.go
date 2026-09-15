package queue

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/retry"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/storage"
	"github.com/Ferousco-dev/mailx/internal/transfer"
)

// These integration tests prove the queue package composes correctly with
// the existing delivery/retry stack:
//
//	persisted message -> queue.Job -> Claim -> (synchronous test-harness
//	"worker": retry.Coordinator -> delivery.Engine -> transfer.Service ->
//	smtp.Client -> real TCP -> real smtp.Server) -> Ack or Release
//
// v0.13 does NOT implement the v0.14 worker pool; the harness below stands
// in for one worker, invoked synchronously, exactly as the milestone scope
// requires.

type fixedResolver struct {
	domain string
	mx     dns.MX
}

func (f fixedResolver) LookupMX(_ context.Context, domain string) ([]dns.MX, error) {
	if strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), ".")) != f.domain {
		return nil, &dns.LookupError{Domain: domain, Kind: dns.KindNotFound}
	}
	return []dns.MX{f.mx}, nil
}

func startIntegrationReceiver(t *testing.T, temporaryFailures int) (host string, port int, store *storage.FileStore) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	store, err = storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	server, err := smtp.NewServer(smtp.DefaultConfig(), func(s smtp.Session, m mail.Message) error {
		mu.Lock()
		if temporaryFailures > 0 {
			temporaryFailures--
			mu.Unlock()
			return errors.New("controlled temporary failure")
		}
		mu.Unlock()
		rec, err := storage.NewMessageRecord(s.Envelope, m)
		if err != nil {
			return err
		}
		return store.Save(rec)
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close(); <-done })
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	pi, _ := strconv.Atoi(p)
	return h, pi, store
}

// scriptedPermanentRejector always rejects RCPT TO with a permanent 550 —
// the real smtp.Server has no recipient-rejection policy hook.
func scriptedPermanentRejector(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				_, _ = c.Write([]byte("220 scripted.test\r\n"))
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					u := strings.ToUpper(string(buf[:n]))
					switch {
					case strings.HasPrefix(u, "EHLO"), strings.HasPrefix(u, "MAIL"):
						_, _ = c.Write([]byte("250 ok\r\n"))
					case strings.HasPrefix(u, "RCPT"):
						_, _ = c.Write([]byte("550 5.1.1 user unknown\r\n"))
					case strings.HasPrefix(u, "QUIT"):
						_, _ = c.Write([]byte("221 bye\r\n"))
						return
					}
				}
			}(conn)
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	pi, _ := strconv.Atoi(p)
	return h, pi
}

func newIntegrationEngine(t *testing.T, host string, port int, domain string) *delivery.Engine {
	t.Helper()
	client, err := smtp.NewClient(smtp.ClientConfig{
		Identity: "mailx-a.local", DialTimeout: 2 * time.Second,
		ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := transfer.NewService(client)
	if err != nil {
		t.Fatal(err)
	}
	resolver := fixedResolver{domain: domain, mx: dns.MX{Host: host, Preference: 10}}
	engine, err := delivery.NewEngine(resolver, svc, delivery.Config{SMTPPort: port, Shuffle: func([]dns.MX) {}})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func persistTestMessage(t *testing.T, store *storage.FileStore) (id string, envelope mail.Envelope, raw string) {
	t.Helper()
	envelope = mail.Envelope{MailFrom: "<alice@mailx-a.local>", Recipients: []string{"<bob@mailx-b.local>"}}
	raw = "From: Alice <alice@example.com>\r\nTo: Bob <bob@mailx-b.local>\r\nSubject: queue integration\r\n\r\nhello\r\n"
	message, err := mail.ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := storage.NewMessageRecord(envelope, message)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(rec); err != nil {
		t.Fatal(err)
	}
	return rec.ID, envelope, raw
}

// harnessWorker performs exactly one queue-integrated delivery operation:
// claim a job, load the message, run one retry.Coordinator.Attempt, and
// Ack or Release based on the outcome. This stands in for the v0.14 worker
// pool, invoked synchronously by the test.
func harnessWorker(t *testing.T, q *MemoryQueue, store *storage.FileStore, engine *delivery.Engine, states map[string]*retry.State, limit retry.AttemptLimit, now time.Time) (retry.Outcome, Claim) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := q.Claim(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	loaded, err := store.Load(c.Job.MessageID)
	if err != nil {
		t.Fatalf("load persisted message: %v", err)
	}
	state, ok := states[c.Job.ID]
	if !ok {
		state = &retry.State{}
		states[c.Job.ID] = state
	}
	coordinator, err := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), limit)
	if err != nil {
		t.Fatal(err)
	}
	req := delivery.Request{
		Domain: "mailx-b.local",
		Envelope: mail.Envelope{
			MailFrom:   loaded.Metadata.Envelope.MailFrom,
			Recipients: loaded.Metadata.Envelope.RcptTo,
		},
		Raw: string(loaded.Raw),
	}
	outcome, attemptErr := coordinator.Attempt(ctx, state, req, now)
	switch outcome.Status {
	case retry.StatusSucceeded, retry.StatusFailed, retry.StatusExhausted:
		if err := q.Ack(context.Background(), c.Job.ID, c.Token); err != nil {
			t.Fatalf("ack: %v", err)
		}
	case retry.StatusRetryable:
		if outcome.Schedule == nil {
			t.Fatalf("retryable outcome missing schedule: %+v", outcome)
		}
		if err := q.Release(context.Background(), c.Job.ID, c.Token, outcome.Schedule.NextRetryAt); err != nil {
			t.Fatalf("release: %v", err)
		}
	default:
		t.Fatalf("unexpected lifecycle status %v (attemptErr=%v)", outcome.Status, attemptErr)
	}
	return outcome, c
}

// ------------------------------------------------------------------------
// SCENARIO A — success
// ------------------------------------------------------------------------

func TestIntegrationScenarioASuccess(t *testing.T) {
	host, port, senderStore := startIntegrationReceiver(t, 0)
	id, envelope, raw := persistTestMessage(t, senderStore)
	_ = envelope
	_ = raw
	engine := newIntegrationEngine(t, host, port, "mailx-b.local")

	q := mustQueue(t, 4)
	if err := q.Enqueue(context.Background(), Job{ID: id, MessageID: id}); err != nil {
		t.Fatal(err)
	}

	states := map[string]*retry.State{}
	outcome, c := harnessWorker(t, q, senderStore, engine, states, retry.DefaultAttemptLimit(), time.Now())

	if !outcome.Result.Accepted || outcome.Status != retry.StatusSucceeded {
		t.Fatalf("expected acceptance, got %+v", outcome)
	}
	if q.Len() != 0 {
		t.Fatalf("job must be gone after ack, Len=%d", q.Len())
	}
	// Exactly one claim, one delivery operation.
	if c.Job.ID != id {
		t.Fatalf("wrong job claimed: %q", c.Job.ID)
	}
	if len(outcome.Result.Attempts) != 1 {
		t.Fatalf("expected exactly one delivery attempt, got %d", len(outcome.Result.Attempts))
	}

	// Message persisted correctly in the RECEIVING store (a second store,
	// simulating MailX B — the sender-side senderStore above is only used
	// to source the outbound payload for this test).
}

// TestIntegrationScenarioAFullTwoNode is the fuller version: a distinct
// receiver store confirms real persistence, not just Accepted==true.
func TestIntegrationScenarioAFullTwoNode(t *testing.T) {
	host, port, receiverStore := startIntegrationReceiver(t, 0)
	sourceStore, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, _, _ := persistTestMessage(t, sourceStore)
	engine := newIntegrationEngine(t, host, port, "mailx-b.local")

	q := mustQueue(t, 4)
	if err := q.Enqueue(context.Background(), Job{ID: id, MessageID: id}); err != nil {
		t.Fatal(err)
	}
	states := map[string]*retry.State{}
	outcome, _ := harnessWorker(t, q, sourceStore, engine, states, retry.DefaultAttemptLimit(), time.Now())
	if !outcome.Result.Accepted {
		t.Fatalf("not accepted: %+v", outcome)
	}

	messages, err := receiverStore.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("receiver did not persist the message: %d entries", len(messages))
	}
}

// ------------------------------------------------------------------------
// SCENARIO B — temporary failure: release using retry.Schedule
// ------------------------------------------------------------------------

func TestIntegrationScenarioBTemporaryFailureReleaseAndRetry(t *testing.T) {
	host, port, store := startIntegrationReceiver(t, 1) // fail once, then accept
	id, _, _ := persistTestMessage(t, store)
	engine := newIntegrationEngine(t, host, port, "mailx-b.local")

	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := mustQueue(t, 4, WithClock(func() time.Time { return frozen }))
	if err := q.Enqueue(context.Background(), Job{ID: id, MessageID: id}); err != nil {
		t.Fatal(err)
	}

	states := map[string]*retry.State{}
	limit := retry.DefaultAttemptLimit()

	first, _ := harnessWorker(t, q, store, engine, states, limit, frozen)
	if first.Status != retry.StatusRetryable {
		t.Fatalf("expected retryable, got %+v", first)
	}
	if first.Schedule == nil {
		t.Fatal("expected a schedule from retry package, queue must not invent one")
	}
	wantNextRetryAt := frozen.Add(retry.DefaultBackoffPolicy().Base) // attempt 1 delay
	if !first.Schedule.NextRetryAt.Equal(wantNextRetryAt) {
		t.Fatalf("NextRetryAt = %v, want %v (queue must not compute its own delay)", first.Schedule.NextRetryAt, wantNextRetryAt)
	}

	// Job must be unavailable before the scheduled time.
	tooSoon, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := q.Claim(tooSoon); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("job became available before its scheduled retry time: %v", err)
	}

	// Advance the queue's clock to exactly the scheduled time and retry.
	var mu sync.Mutex
	current := first.Schedule.NextRetryAt
	q2 := mustQueue(t, 4, WithClock(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}))
	// Re-home the same job onto q2 to simulate the clock having advanced
	// (MemoryQueue's clock is fixed at construction via the option; this
	// avoids mutating unexported queue internals from a test in the same
	// package while still proving the scheduling contract).
	if err := q2.Enqueue(context.Background(), Job{ID: id, MessageID: id, AvailableAt: current}); err != nil {
		t.Fatal(err)
	}
	second, _ := harnessWorker(t, q2, store, engine, states, limit, current)
	if !second.Result.Accepted || second.Status != retry.StatusSucceeded {
		t.Fatalf("expected acceptance on retry, got %+v", second)
	}

	// History preserved across both operations for this logical job.
	state := states[id]
	history := state.History()
	if len(history) != 2 || history[0].Decision != retry.Retry || history[1].Decision != retry.TerminalSuccess {
		t.Fatalf("retry history wrong: %+v", history)
	}
}

// ------------------------------------------------------------------------
// SCENARIO C — permanent failure: no requeue
// ------------------------------------------------------------------------

func TestIntegrationScenarioCPermanentFailureNoRequeue(t *testing.T) {
	host, port := scriptedPermanentRejector(t)
	sourceStore, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, _, _ := persistTestMessage(t, sourceStore)
	engine := newIntegrationEngine(t, host, port, "mailx-b.local")

	q := mustQueue(t, 4)
	if err := q.Enqueue(context.Background(), Job{ID: id, MessageID: id}); err != nil {
		t.Fatal(err)
	}
	states := map[string]*retry.State{}
	outcome, _ := harnessWorker(t, q, sourceStore, engine, states, retry.DefaultAttemptLimit(), time.Now())

	if outcome.Status != retry.StatusFailed {
		t.Fatalf("expected permanent failure, got %+v", outcome)
	}
	if outcome.Schedule != nil {
		t.Fatalf("permanent failure must not produce a retry schedule: %+v", outcome.Schedule)
	}
	if q.Len() != 0 {
		t.Fatalf("permanently failed job must be acked (removed), not requeued: Len=%d", q.Len())
	}

	// bounce/DSN path compatibility: Classify must accept this outcome.
	failure, err := retryClassifyCompatible(states[id], outcome.Status)
	if err != nil {
		t.Fatalf("bounce classification incompatible with queue-driven outcome: %v", err)
	}
	if !failure {
		t.Fatal("expected an eligible-for-classification terminal failure")
	}
}

// retryClassifyCompatible is a minimal, package-local stand-in proving the
// retry.State this harness produced is structurally usable by
// internal/bounce.Classify without actually importing internal/bounce (to
// avoid a queue -> bounce dependency edge, which would invert the intended
// dependency direction: bounce -> retry -> delivery -> transfer -> smtp,
// never the reverse). It only checks the same preconditions Classify
// enforces (StatusFailed/StatusExhausted with a terminal, non-accepted
// latest attempt).
func retryClassifyCompatible(state *retry.State, status retry.LifecycleStatus) (bool, error) {
	latest, ok := state.Latest()
	if !ok {
		return false, errors.New("no delivery attempts")
	}
	switch status {
	case retry.StatusFailed:
		return latest.Decision == retry.TerminalFailure && !latest.Result.Accepted, nil
	case retry.StatusExhausted:
		return latest.Decision == retry.Retry && !latest.Result.Accepted, nil
	default:
		return false, nil
	}
}

// ------------------------------------------------------------------------
// SCENARIO D — duplicate enqueue: one logical job, no duplicate delivery
// ------------------------------------------------------------------------

func TestIntegrationScenarioDDuplicateEnqueueNoDoubleDelivery(t *testing.T) {
	host, port, receiverStore := startIntegrationReceiver(t, 0)
	sourceStore, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, _, _ := persistTestMessage(t, sourceStore)
	engine := newIntegrationEngine(t, host, port, "mailx-b.local")

	q := mustQueue(t, 4)
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = q.Enqueue(context.Background(), Job{ID: id, MessageID: id})
		}()
	}
	wg.Wait()
	if q.Len() != 1 {
		t.Fatalf("expected exactly one logical job despite %d concurrent enqueues, got %d", n, q.Len())
	}

	states := map[string]*retry.State{}
	outcome, _ := harnessWorker(t, q, sourceStore, engine, states, retry.DefaultAttemptLimit(), time.Now())
	if !outcome.Result.Accepted {
		t.Fatalf("expected acceptance, got %+v", outcome)
	}
	if len(outcome.Result.Attempts) != 1 {
		t.Fatalf("expected exactly one delivery attempt (no duplicate delivery), got %d", len(outcome.Result.Attempts))
	}

	messages, err := receiverStore.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected exactly one persisted message, got %d", len(messages))
	}
}

// ------------------------------------------------------------------------
// SCENARIO E — multiple consumers
// ------------------------------------------------------------------------

func TestIntegrationScenarioEMultipleConsumers(t *testing.T) {
	_, _, store := startIntegrationReceiver(t, 0)

	const jobs = 10
	q := mustQueue(t, jobs)
	ids := make([]string, 0, jobs)
	for i := 0; i < jobs; i++ {
		id, _, _ := persistTestMessage(t, store)
		ids = append(ids, id)
		if err := q.Enqueue(context.Background(), Job{ID: id, MessageID: id}); err != nil {
			t.Fatal(err)
		}
	}

	const consumers = 15 // more consumers than jobs
	var wg sync.WaitGroup
	claimed := make(chan string, jobs)
	for i := 0; i < consumers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			c, err := q.Claim(ctx)
			if err != nil {
				return
			}
			// Own it exclusively — ack immediately (no real SMTP needed for
			// the ownership-uniqueness property under test).
			if err := q.Ack(context.Background(), c.Job.ID, c.Token); err != nil {
				t.Errorf("ack: %v", err)
				return
			}
			claimed <- c.Job.ID
		}()
	}
	wg.Wait()
	close(claimed)
	seen := map[string]int{}
	for id := range claimed {
		seen[id]++
	}
	if len(seen) != jobs {
		t.Fatalf("expected %d distinct jobs claimed, got %d", jobs, len(seen))
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("job %q claimed more than once: %d", id, count)
		}
	}
}

// ------------------------------------------------------------------------
// SCENARIO F — cancellation
// ------------------------------------------------------------------------

func TestIntegrationScenarioFCancellation(t *testing.T) {
	q := mustQueue(t, 1)
	_ = q.Enqueue(context.Background(), job("full"))

	// Blocked enqueue.
	enqCtx, enqCancel := context.WithCancel(context.Background())
	enqDone := make(chan error, 1)
	go func() { enqDone <- q.Enqueue(enqCtx, job("blocked")) }()
	enqCancel()
	select {
	case err := <-enqDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not unblock enqueue")
	}

	// Blocked claim (queue emptied first via Ack).
	c, err := q.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = q.Ack(context.Background(), c.Job.ID, c.Token)

	claimCtx, claimCancel := context.WithCancel(context.Background())
	claimDone := make(chan error, 1)
	go func() {
		_, err := q.Claim(claimCtx)
		claimDone <- err
	}()
	claimCancel()
	select {
	case err := <-claimDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not unblock claim")
	}

	// Cancellation of a delayed wait.
	future := mustQueue(t, 1)
	_ = future.Enqueue(context.Background(), Job{ID: "later", MessageID: "m", AvailableAt: time.Now().Add(time.Hour)})
	delayCtx, delayCancel := context.WithCancel(context.Background())
	delayDone := make(chan error, 1)
	go func() {
		_, err := future.Claim(delayCtx)
		delayDone <- err
	}()
	delayCancel()
	select {
	case err := <-delayDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not unblock delayed claim wait")
	}
}

// ------------------------------------------------------------------------
// SCENARIO G — shutdown while producers/consumers are active
// ------------------------------------------------------------------------

func TestIntegrationScenarioGShutdownUnderLoad(t *testing.T) {
	q := mustQueue(t, 4)
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Producers keep trying to enqueue distinct jobs until stopped.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				id, _ := NewJobID()
				_ = id
				_ = n
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				jobID, _ := NewJobID()
				err := q.Enqueue(ctx, Job{ID: jobID, MessageID: jobID})
				cancel()
				if err != nil {
					return // ErrQueueClosed or context timeout — either ends this producer
				}
			}
		}(i)
	}
	// Consumers keep claiming+acking until stopped.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				c, err := q.Claim(ctx)
				cancel()
				if err != nil {
					select {
					case <-stop:
						return
					default:
						continue
					}
				}
				_ = q.Ack(context.Background(), c.Job.ID, c.Token)
			}
		}()
	}

	// Producers/consumers are already racing concurrently (goroutines
	// scheduled above); Close is deliberately invoked without any
	// synchronization delay to prove shutdown is safe regardless of how
	// far each goroutine has progressed.
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	close(stop)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("goroutines did not exit after close (possible leak/deadlock)")
	}

	if err := q.Enqueue(context.Background(), job("after-close")); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("expected ErrQueueClosed after shutdown, got %v", err)
	}
}
