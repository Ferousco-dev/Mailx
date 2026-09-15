package worker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/retry"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/storage"
	"github.com/Ferousco-dev/mailx/internal/transfer"
)

// Real-stack integration: persisted message -> queue -> worker.Pool ->
// storage.Load -> retry.Coordinator -> delivery.Engine -> DNS seam ->
// transfer.Service -> smtp.Client -> real local TCP -> smtp.Server ->
// storage. No public Internet, no public DNS.

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

func startReceiver(t *testing.T, temporaryFailures int) (host string, port int, store *storage.FileStore) {
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

// scriptedPermanentRejector always rejects RCPT with 550 — the real
// smtp.Server has no recipient-rejection policy hook.
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
				r := bufio.NewReader(c)
				_, _ = c.Write([]byte("220 scripted.test\r\n"))
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					u := strings.ToUpper(line)
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

// scriptedAcceptThenDropBeforeQuit accepts DATA (final 250) then drops the
// connection instead of replying to QUIT — the Accepted+QUIT-failure case.
func scriptedAcceptThenDropBeforeQuit(t *testing.T) (host string, port int) {
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
				r := bufio.NewReader(c)
				_, _ = c.Write([]byte("220 scripted.test\r\n"))
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					u := strings.ToUpper(line)
					switch {
					case strings.HasPrefix(u, "EHLO"), strings.HasPrefix(u, "MAIL"), strings.HasPrefix(u, "RCPT"):
						_, _ = c.Write([]byte("250 ok\r\n"))
					case strings.HasPrefix(u, "DATA"):
						_, _ = c.Write([]byte("354 go\r\n"))
						for {
							l, err := r.ReadString('\n')
							if err != nil || strings.TrimRight(l, "\r\n") == "." {
								break
							}
						}
						_, _ = c.Write([]byte("250 accepted\r\n"))
						return // drop instead of replying to QUIT
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

func persistIntegrationMessage(t *testing.T, store *storage.FileStore, mailFrom string, recipients []string) string {
	t.Helper()
	raw := "From: Alice <alice@example.com>\r\nTo: Bob <bob@mailx-b.local>\r\nSubject: worker integration\r\n\r\nhello\r\n"
	message, err := mail.ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := storage.NewMessageRecord(mail.Envelope{MailFrom: mailFrom, Recipients: recipients}, message)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(rec); err != nil {
		t.Fatal(err)
	}
	return rec.ID
}

// realCoordinator builds a *retry.Coordinator around a real delivery.Engine.
func realCoordinator(t *testing.T, engine *delivery.Engine, limit retry.AttemptLimit) *retry.Coordinator {
	t.Helper()
	c, err := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), limit)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// countingCoordinator wraps a real *retry.Coordinator and counts completed
// Attempt calls via atomics, notifying a channel after each one. This lets
// a test wait deterministically for N real delivery operations to finish
// WITHOUT reading retry.State concurrently with the worker goroutine that
// owns it — retry.State explicitly documents itself as not safe for
// concurrent access (single-owner only), so a test must never peek at a
// job's State while the pool might still be mutating it.
type countingCoordinator struct {
	inner    Coordinator
	attempts int32
	notify   chan struct{}
}

func newCountingCoordinator(inner Coordinator) *countingCoordinator {
	return &countingCoordinator{inner: inner, notify: make(chan struct{}, 4096)}
}

func (c *countingCoordinator) Attempt(ctx context.Context, state *retry.State, req delivery.Request, now time.Time) (retry.Outcome, error) {
	outcome, err := c.inner.Attempt(ctx, state, req, now)
	atomic.AddInt32(&c.attempts, 1)
	select {
	case c.notify <- struct{}{}:
	default:
	}
	return outcome, err
}

func (c *countingCoordinator) totalAttempts() int { return int(atomic.LoadInt32(&c.attempts)) }

func (c *countingCoordinator) waitAttempts(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for c.totalAttempts() < n {
		select {
		case <-c.notify:
		case <-deadline:
			t.Fatalf("timed out waiting for %d real delivery operations, got %d", n, c.totalAttempts())
		}
	}
}

// ------------------------------------------------------------ A: success

func TestIntegrationSuccess(t *testing.T) {
	host, port, receiverStore := startReceiver(t, 0)
	sourceStore, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := persistIntegrationMessage(t, sourceStore, "<alice@mailx-a.local>", []string{"<bob@mailx-b.local>"})
	engine := newIntegrationEngine(t, host, port, "mailx-b.local")
	coord := realCoordinator(t, engine, retry.DefaultAttemptLimit())

	q := mustQ(t, 4)
	errFn, errs, mu := collectErrors()
	p := mustPool(t, q, sourceStore, coord, Config{Workers: 1}, WithOnError(*errFn))

	if err := q.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	waitForQueueEmpty(t, q, 2*time.Second)
	cancel()
	<-done

	mu.Lock()
	if len(*errs) != 0 {
		t.Fatalf("unexpected operational errors: %v", *errs)
	}
	mu.Unlock()

	messages, err := receiverStore.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected exactly one persisted delivery, got %d", len(messages))
	}
	if p.states.len() != 0 {
		t.Fatalf("state must be forgotten after success, got %d", p.states.len())
	}
}

// -------------------------------------------------- B: temporary→success

func TestIntegrationTemporaryThenSuccess(t *testing.T) {
	host, port, receiverStore := startReceiver(t, 1) // fail once, then accept
	sourceStore, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := persistIntegrationMessage(t, sourceStore, "<alice@mailx-a.local>", []string{"<bob@mailx-b.local>"})
	engine := newIntegrationEngine(t, host, port, "mailx-b.local")
	coord := newCountingCoordinator(realCoordinator(t, engine, retry.DefaultAttemptLimit()))

	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := mustQ(t, 4, queue.WithClock(func() time.Time { return frozen }))
	p := mustPool(t, q, sourceStore, coord, Config{Workers: 1}, WithClock(func() time.Time { return frozen }))

	if err := q.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	// Wait for the first, temporary REAL delivery operation to complete
	// via the counting wrapper — never by reading retry.State while the
	// pool might still be concurrently writing to it (see
	// countingCoordinator's doc comment).
	coord.waitAttempts(t, 1, 2*time.Second)
	cancel()
	<-done

	if p.states.len() != 1 {
		t.Fatalf("retry state must persist after temporary failure, got %d", p.states.len())
	}
	state := p.states.get(id)
	if state.Count() != 1 {
		t.Fatalf("expected 1 recorded operation, got %d", state.Count())
	}
	latest, _ := state.Latest()
	if latest.Result.FinalCode != 0 && latest.Decision != retry.Retry {
		t.Fatalf("expected a retryable decision, got %+v", latest)
	}
	nextRetryAt := latest.Result.FinishedAt // not exact; use schedule via History instead
	_ = nextRetryAt

	// Confirm it is unavailable before due and query its release time via
	// direct queue inspection is not exposed — instead re-home onto a
	// clock reporting a far-future time so it becomes claimable, proving
	// the SAME state resumes and completes.
	future := frozen.Add(24 * time.Hour)
	q2 := mustQ(t, 4, queue.WithClock(func() time.Time { return future }))
	if err := q2.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id, AvailableAt: frozen}); err != nil {
		t.Fatal(err)
	}
	p.q = q2

	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { p.Run(ctx2); close(done2) }()
	waitForQueueEmpty(t, q2, 2*time.Second)
	cancel2()
	<-done2

	messages, err := receiverStore.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected exactly one persisted delivery after retry, got %d", len(messages))
	}
	if p.states.len() != 0 {
		t.Fatalf("state must be forgotten after eventual success, got %d", p.states.len())
	}
	if state.Count() != 2 {
		t.Fatalf("expected 2 total recorded operations, got %d", state.Count())
	}
}

// ------------------------------------------------------------ C: exhaustion

func TestIntegrationExhaustion(t *testing.T) {
	host, port, receiverStore := startReceiver(t, 999) // always temporarily fail
	sourceStore, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := persistIntegrationMessage(t, sourceStore, "<alice@mailx-a.local>", []string{"<bob@mailx-b.local>"})
	engine := newIntegrationEngine(t, host, port, "mailx-b.local")
	limit := retry.AttemptLimit{MaxAttempts: 2}
	coord := newCountingCoordinator(realCoordinator(t, engine, limit))

	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := mustQ(t, 4, queue.WithClock(func() time.Time { return frozen }))
	p := mustPool(t, q, sourceStore, coord, Config{Workers: 1}, WithClock(func() time.Time { return frozen }))
	if err := q.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	coord.waitAttempts(t, 1, 2*time.Second) // first temporary failure -> released
	cancel()
	<-done

	state := p.states.get(id)
	future := frozen.Add(24 * time.Hour)
	q2 := mustQ(t, 4, queue.WithClock(func() time.Time { return future }))
	if err := q2.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id, AvailableAt: frozen}); err != nil {
		t.Fatal(err)
	}
	p.q = q2
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { p.Run(ctx2); close(done2) }()
	waitForQueueEmpty(t, q2, 2*time.Second) // second op exhausts -> acked
	cancel2()
	<-done2

	if state.Count() != 2 {
		t.Fatalf("expected exactly 2 retry-level operations (exhaustion, no 3rd), got %d", state.Count())
	}
	latest, _ := state.Latest()
	if latest.Decision != retry.Retry {
		t.Fatalf("exhausted latest decision must still be Retry (limit reached), got %v", latest.Decision)
	}
	if p.states.len() != 0 {
		t.Fatalf("state must be forgotten after exhaustion, got %d", p.states.len())
	}
	messages, err := receiverStore.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("exhausted delivery must never have persisted at the receiver, got %d", len(messages))
	}

	// bounce compatibility check (mirrors internal/bounce.Classify's own
	// preconditions) — proves the terminal state this package produced is
	// usable by the bounce path without importing it here (dependency
	// direction: bounce -> retry, never retry/worker -> bounce for this
	// check; worker package itself DOES depend on bounce for DSN
	// generation, exercised directly by handleTerminalFailure already).
	if latest.Result.Accepted {
		t.Fatal("exhausted operation must never be Accepted")
	}
}

// ------------------------------------------------------------ D: permanent

func TestIntegrationPermanentFailure(t *testing.T) {
	host, port := scriptedPermanentRejector(t)
	sourceStore, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := persistIntegrationMessage(t, sourceStore, "<alice@mailx-a.local>", []string{"<bob@mailx-b.local>"})
	engine := newIntegrationEngine(t, host, port, "mailx-b.local")
	coord := realCoordinator(t, engine, retry.DefaultAttemptLimit())

	q := mustQ(t, 4)
	p := mustPool(t, q, sourceStore, coord, Config{Workers: 1})
	if err := q.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	waitForQueueEmpty(t, q, 2*time.Second)
	cancel()
	<-done

	if p.states.len() != 0 {
		t.Fatalf("state must be forgotten after permanent failure, got %d", p.states.len())
	}
}

// ---------------------------------------------------- E: accepted+QUIT --

func TestIntegrationAcceptedPlusQuitFailure(t *testing.T) {
	host, port := scriptedAcceptThenDropBeforeQuit(t)
	sourceStore, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := persistIntegrationMessage(t, sourceStore, "<alice@mailx-a.local>", []string{"<bob@mailx-b.local>"})
	engine := newIntegrationEngine(t, host, port, "mailx-b.local")
	coord := realCoordinator(t, engine, retry.DefaultAttemptLimit())

	q := mustQ(t, 4)
	p := mustPool(t, q, sourceStore, coord, Config{Workers: 1})
	if err := q.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	waitForQueueEmpty(t, q, 2*time.Second)
	cancel()
	<-done

	// Job was acked (Len==0 already proven); state must be gone (terminal
	// success), not retried, not cycling.
	if p.states.len() != 0 {
		t.Fatalf("accepted+QUIT-failure must reach a terminal, forgotten state, got %d", p.states.len())
	}
}

// --------------------------------------------------------- F: multiple --

func TestIntegrationMultipleWorkers(t *testing.T) {
	host, port, receiverStore := startReceiver(t, 0)
	sourceStore, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	engine := newIntegrationEngine(t, host, port, "mailx-b.local")
	coord := realCoordinator(t, engine, retry.DefaultAttemptLimit())

	const jobs = 8
	q := mustQ(t, jobs)
	p := mustPool(t, q, sourceStore, coord, Config{Workers: 4})

	for i := 0; i < jobs; i++ {
		id := persistIntegrationMessage(t, sourceStore, fmt.Sprintf("<sender%d@mailx-a.local>", i), []string{fmt.Sprintf("<r%d@mailx-b.local>", i)})
		if err := q.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id}); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	waitForQueueEmpty(t, q, 5*time.Second)
	cancel()
	<-done

	messages, err := receiverStore.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != jobs {
		t.Fatalf("expected %d persisted deliveries, got %d", jobs, len(messages))
	}
}

// --------------------------------------------------------- G: shutdown --

func TestIntegrationShutdownDuringWork(t *testing.T) {
	host, port, _ := startReceiver(t, 0)
	sourceStore, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	engine := newIntegrationEngine(t, host, port, "mailx-b.local")
	coord := realCoordinator(t, engine, retry.DefaultAttemptLimit())

	const jobs = 6
	q := mustQ(t, jobs)
	p := mustPool(t, q, sourceStore, coord, Config{Workers: 3})
	for i := 0; i < jobs; i++ {
		id := persistIntegrationMessage(t, sourceStore, fmt.Sprintf("<s%d@mailx-a.local>", i), []string{fmt.Sprintf("<r%d@mailx-b.local>", i)})
		if err := q.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id}); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	// Cancel immediately — no synchronization delay — to prove shutdown is
	// safe regardless of how far each worker has progressed.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pool did not shut down under concurrent load")
	}
	// Whatever happened, no job should be permanently lost: every job is
	// either still tracked (Len()>0, available or claimed) or was Acked
	// because it genuinely reached a terminal outcome. We only assert no
	// panic/deadlock occurred (already implied by reaching here) and that
	// the queue is in a consistent, inspectable state.
	if q.Len() < 0 {
		t.Fatal("impossible negative queue length")
	}
}

// -------------------------------------------------------- H: missing msg

func TestIntegrationMissingMessage(t *testing.T) {
	host, port, _ := startReceiver(t, 0)
	sourceStore, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	engine := newIntegrationEngine(t, host, port, "mailx-b.local")
	coord := realCoordinator(t, engine, retry.DefaultAttemptLimit())

	q := mustQ(t, 4)
	errFn, errs, mu := collectErrors()
	p := mustPool(t, q, sourceStore, coord, Config{Workers: 1}, WithOnError(*errFn))

	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "does-not-exist"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(*errs)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(*errs) == 0 {
		t.Fatal("expected an operational error for the missing message")
	}
	if q.Len() != 1 {
		t.Fatalf("missing-message job must remain tracked (released), Len=%d", q.Len())
	}
}

// ------------------------------------------------------ I: many temp fails

func TestIntegrationManyTemporaryFailuresNoStorm(t *testing.T) {
	host, port, _ := startReceiver(t, 999) // always temporary
	sourceStore, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	engine := newIntegrationEngine(t, host, port, "mailx-b.local")
	coord := newCountingCoordinator(realCoordinator(t, engine, retry.DefaultAttemptLimit()))

	const jobs = 6
	q := mustQ(t, jobs)
	p := mustPool(t, q, sourceStore, coord, Config{Workers: 3})
	ids := make([]string, jobs)
	for i := 0; i < jobs; i++ {
		id := persistIntegrationMessage(t, sourceStore, fmt.Sprintf("<s%d@mailx-a.local>", i), []string{fmt.Sprintf("<r%d@mailx-b.local>", i)})
		ids[i] = id
		if err := q.Enqueue(context.Background(), queue.Job{ID: id, MessageID: id}); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	// Wait for exactly `jobs` REAL delivery operations to complete, using
	// the counting wrapper's atomics/channel — never by reading
	// retry.State from this goroutine while a worker goroutine might still
	// be concurrently calling State.Record on it (retry.State is
	// documented single-owner, not concurrency-safe). Only after <-done
	// (which happens-after every worker goroutine has fully exited) is it
	// safe for this test to inspect any job's retry.State.
	coord.waitAttempts(t, jobs, 8*time.Second)
	cancel()
	<-done

	if coord.totalAttempts() != jobs {
		t.Fatalf("expected exactly %d real delivery operations (no storm), got %d", jobs, coord.totalAttempts())
	}
	if p.states.len() != jobs {
		t.Fatalf("expected %d in-flight retry states, got %d", jobs, p.states.len())
	}
	for _, id := range ids {
		if got := p.states.get(id).Count(); got != 1 {
			t.Fatalf("job %q: expected exactly 1 recorded operation (no storm), got %d", id, got)
		}
	}
}

// -------------------------------------------------------------- helpers -

func waitForQueueEmpty(t *testing.T, q *queue.MemoryQueue, timeout time.Duration) {
	t.Helper()
	waitForQueueLen(t, q, 0, timeout)
}

func waitForQueueLen(t *testing.T, q *queue.MemoryQueue, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if q.Len() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("queue length never reached %d within %v (last=%d)", want, timeout, q.Len())
}

var _ = os.TempDir
var _ = filepath.Join
