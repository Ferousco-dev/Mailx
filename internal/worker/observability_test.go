package worker

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/buildinfo"
	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/observability"
	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/retry"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

const (
	privRcpt   = "PRIVATE-RCPT-MARKER"
	privDomain = "private-domain-marker.example"
	privRemote = "REMOTE-SMTP-TEXT-MARKER"
)

func obsPool(t *testing.T, q queue.Queue, l Loader, c Coordinator, store OutcomeStore, opts ...Option) (*Pool, *syncBuf, *observability.Metrics) {
	t.Helper()
	buf := &syncBuf{}
	logger, err := observability.NewLogger(buf, "debug", "json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := observability.NewMetrics(buildinfo.Info{Version: "t", Commit: "t"})
	if err != nil {
		t.Fatal(err)
	}
	opts = append(opts, WithLogger(logger), WithMetrics(m))
	p, err := NewPool(q, l, c, store, Config{Workers: 1}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return p, buf, m
}

func assertNoPrivateMarkers(t *testing.T, out string) {
	t.Helper()
	for _, marker := range []string{privRcpt, privDomain, privRemote, "Subject: x", "body"} {
		if strings.Contains(out, marker) {
			t.Fatalf("log leaked %q:\n%s", marker, out)
		}
	}
}

func TestWorkerLogsSuccessCorrelationAndMetrics(t *testing.T) {
	q := mustQ(t, 4)
	l := newFakeLoader()
	from := "<a@" + privDomain + ">"
	l.put("m1", envelopeFor(from, "<"+privRcpt+"@"+privDomain+">"), "Subject: x\r\n\r\nbody\r\n")
	c := newScriptedCoordinator()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.script(from, coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{
		Accepted: true, Kind: delivery.KindAccepted, FinalCode: 250, StartedAt: start, FinishedAt: start.Add(1500 * time.Millisecond),
		RemoteMessage: privRemote}}})
	p, buf, m := obsPool(t, q, l, c, newFakeOutcomeStore())
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	runPoolUntilIdle(t, p, q, 2*time.Second)
	out := buf.String()
	for _, want := range []string{`"msg":"queue_claimed"`, `"msg":"delivery_outcome"`, `"msg":"queue_acked"`, `"job_id":"j1"`,
		`"message_id":"m1"`, `"attempt":1`, `"kind":"accepted"`, `"decision":"terminal_success"`, `"accepted":true`, `"smtp_code":250`, `"duration_ms":1500`} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %s:\n%s", want, out)
		}
	}
	assertNoPrivateMarkers(t, out)
	rec := m.Registry()
	if _, err := rec.Gather(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerLogsRetryAndTerminalReclaim(t *testing.T) {
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := mustQ(t, 4, queue.WithClock(func() time.Time { return frozen }))
	l := newFakeLoader()
	l.put("m1", envelopeFor("<temp@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator()
	sched := retry.Schedule{Attempt: 1, Delay: time.Minute, NextRetryAt: frozen.Add(time.Minute)}
	c.script("<temp@x>", coordOutcome{outcome: retry.Outcome{Status: retry.StatusRetryable, Schedule: &sched,
		Result: delivery.Result{Kind: delivery.KindTransferTemporary, FinalCode: 451, RemoteMessage: privRemote}}})
	p, buf, _ := obsPool(t, q, l, c, newFakeOutcomeStore(), WithClock(func() time.Time { return frozen }))
	_ = q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"})
	runPoolForAttempts(t, p, q, 1, 2*time.Second)
	out := buf.String()
	if !strings.Contains(out, `"msg":"queue_released"`) || !strings.Contains(out, `"decision":"retry"`) || !strings.Contains(out, `"smtp_code":451`) {
		t.Fatalf("retry not logged:\n%s", out)
	}
	assertNoPrivateMarkers(t, out)

	q2 := mustQ(t, 2)
	store := newFakeOutcomeStore()
	store.terminal["m2"] = true
	p2, buf2, _ := obsPool(t, q2, newFakeLoader(), newScriptedCoordinator(), store)
	_ = q2.Enqueue(context.Background(), queue.Job{ID: "j2", MessageID: "m2"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p2.Run(ctx); close(done) }()
	waitForQueueEmpty(t, q2, 2*time.Second)
	cancel()
	<-done
	if o := buf2.String(); !strings.Contains(o, `"msg":"terminal_reclaim"`) || !strings.Contains(o, `"job_id":"j2"`) {
		t.Fatalf("terminal reclaim not logged:\n%s", o)
	}
}

func TestWorkerMalformedRecipientErrorDoesNotContainAddress(t *testing.T) {
	q := mustQ(t, 4)
	l := newFakeLoader()
	l.put("m1", envelopeFor("<a@x>", privRcpt+"-no-angle-brackets"), "x\r\n")
	errFn, errs, mu := collectErrors()
	p, buf, _ := obsPool(t, q, l, newScriptedCoordinator(), newFakeOutcomeStore(), WithOnError(*errFn))
	_ = q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"})
	runPoolUntilIdleOnce(t, p, q, 2*time.Second)
	mu.Lock()
	defer mu.Unlock()
	for _, e := range *errs {
		if strings.Contains(e.Error(), privRcpt) {
			t.Fatalf("operational error leaked recipient: %v", e)
		}
	}
	assertNoPrivateMarkers(t, buf.String())
}

func TestWorkerPanicIsLoggedWithIDsOnly(t *testing.T) {
	q := mustQ(t, 4)
	l := newFakeLoader()
	l.put("m1", envelopeFor("<panic@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator()
	c.script("<panic@x>", coordOutcome{panic: privRemote})
	p, buf, _ := obsPool(t, q, l, c, newFakeOutcomeStore())
	_ = q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"})
	runPoolForAttempts(t, p, q, 1, 2*time.Second)
	out := buf.String()
	if !strings.Contains(out, `"msg":"worker_panic"`) || !strings.Contains(out, `"job_id":"j1"`) {
		t.Fatalf("panic not logged:\n%s", out)
	}
	assertNoPrivateMarkers(t, out)
}

func TestWorkerBehaviorUnchangedByPanickingLogger(t *testing.T) {
	q := mustQ(t, 4)
	l := newFakeLoader()
	l.put("m1", envelopeFor("<a@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator()
	c.script("<a@x>", coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true, Kind: delivery.KindAccepted}}})
	logger, _ := observability.NewLogger(panicWriter{}, "debug", "json")
	p := mustPool(t, q, l, c, Config{Workers: 1}, WithLogger(logger), WithMetrics(nil))
	_ = q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"})
	runPoolUntilIdle(t, p, q, 2*time.Second)
	if q.Len() != 0 || c.totalAttempts() != 1 {
		t.Fatalf("panicking log sink changed behavior: len=%d attempts=%d", q.Len(), c.totalAttempts())
	}
}

type panicWriter struct{}

func (panicWriter) Write([]byte) (int, error) { panic("disk on fire") }

func TestWorkerLogsPersistenceFailureAndRenewOutcome(t *testing.T) {
	q := mustQ(t, 2)
	l := newFakeLoader()
	l.put("m1", envelopeFor("<a@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator()
	c.script("<a@x>", coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true, Kind: delivery.KindAccepted}}})
	store := newFakeOutcomeStore()
	var calls int
	store.persist = func(context.Context, string, retry.DeliveryAttempt, retry.Outcome) error {
		calls++
		if calls == 1 {
			return errors.New("db down: " + privRemote)
		}
		return nil
	}
	p, buf, m := obsPool(t, q, l, c, store)
	_ = q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	waitForQueueEmpty(t, q, 3*time.Second)
	cancel()
	<-done
	out := buf.String()
	if !strings.Contains(out, `"msg":"outcome_persist_failed"`) || !strings.Contains(out, `"attempt":1`) || !strings.Contains(out, `"msg":"queue_acked"`) {
		t.Fatalf("persistence failure not attributable:\n%s", out)
	}
	if strings.Contains(out, privRemote) {
		t.Fatalf("log leaked underlying error text:\n%s", out)
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `mailx_queue_operations_total{operation="persist",result="error"} 1`) ||
		!strings.Contains(rec.Body.String(), `mailx_delivery_attempts_total{decision="terminal_success",kind="accepted"} 1`) {
		t.Fatalf("metrics missing persist/delivery counters:\n%s", rec.Body.String())
	}
}

// flakyQueue fails Claim for the first n calls, as a Redis outage would.
type flakyQueue struct {
	queue.Queue
	mu    sync.Mutex
	fails int
}

func (f *flakyQueue) Claim(ctx context.Context) (queue.Claim, error) {
	f.mu.Lock()
	if f.fails > 0 {
		f.fails--
		f.mu.Unlock()
		return queue.Claim{}, errors.New("dial tcp: connection refused")
	}
	f.mu.Unlock()
	return f.Queue.Claim(ctx)
}

func TestWorkerSurvivesTransientQueueClaimFailureAndRecovers(t *testing.T) {
	base := mustQ(t, 4)
	l := newFakeLoader()
	l.put("m1", envelopeFor("<a@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator()
	c.script("<a@x>", coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true, Kind: delivery.KindAccepted}}})
	errFn, errs, mu := collectErrors()
	q := &flakyQueue{Queue: base, fails: 1}
	p, buf, _ := obsPool(t, q, l, c, newFakeOutcomeStore(), WithOnError(*errFn))
	_ = base.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	waitForQueueEmpty(t, base, 5*time.Second)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(*errs) != 1 || !strings.Contains(buf.String(), `"msg":"queue_claim_failed"`) || c.totalAttempts() != 1 {
		t.Fatalf("worker must survive one claim failure and then deliver: errs=%v attempts=%d\n%s", *errs, c.totalAttempts(), buf.String())
	}
	if strings.Contains(buf.String(), "connection refused") {
		t.Fatal("claim error text must not reach the structured log")
	}
}
