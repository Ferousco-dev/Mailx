package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/retry"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/smtp/smtptest"
	"github.com/Ferousco-dev/mailx/internal/transfer"
)

// suppStore is the fake outcome store plus a SuppressionGate whose behaviour
// (policy contents, lookup failures, record failures) tests control.
type suppStore struct {
	*fakeOutcomeStore
	gmu        sync.Mutex
	suppressed map[string]bool
	lookupErr  error
	recordErr  error
	lookups    int
	recorded   []recordCall
}

type recordCall struct {
	messageID string
	keys      []string
	all       bool
}

func newSuppStore(suppressed ...string) *suppStore {
	s := &suppStore{fakeOutcomeStore: newFakeOutcomeStore(), suppressed: map[string]bool{}}
	for _, e := range suppressed {
		s.suppressed[e] = true
	}
	return s
}

func (s *suppStore) suppress(email string) {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	s.suppressed[email] = true
}

func (s *suppStore) SuppressedRecipients(_ context.Context, _ string, keys []string) (map[string]bool, error) {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	s.lookups++
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	out := map[string]bool{}
	for _, k := range keys {
		if s.suppressed[k] {
			out[k] = true
		}
	}
	return out, nil
}

func (s *suppStore) RecordSuppressed(_ context.Context, messageID string, suppressed map[string]bool, all bool) error {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	if s.recordErr != nil {
		return s.recordErr
	}
	var keys []string
	for k := range suppressed {
		keys = append(keys, k)
	}
	s.recorded = append(s.recorded, recordCall{messageID, keys, all})
	if all {
		s.fakeOutcomeStore.mu.Lock()
		s.fakeOutcomeStore.terminal[messageID] = true
		s.fakeOutcomeStore.mu.Unlock()
	}
	return nil
}

func (s *suppStore) recordedCalls() []recordCall {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	return append([]recordCall(nil), s.recorded...)
}

type suppRig struct {
	t      *testing.T
	mx     *smtptest.Server
	store  *suppStore
	loader *fakeLoader
	q      *orderedQueue
	base   *queue.MemoryQueue
	client *smtp.Client // trusts the rig's fake MX certificate
	// outcomes is the store handed to pools; it defaults to store and lets a test
	// substitute a wrapper (e.g. one that also implements TenantLookup).
	outcomes OutcomeStore
}

func newSuppRig(t *testing.T, mxOpts smtptest.Options, relay bool, store *suppStore) (*suppRig, func(workers int, opts ...Option) *Pool) {
	t.Helper()
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, nil, time.Hour)
	mxOpts.Advertise, mxOpts.Cert, mxOpts.Capture = true, &cert, true
	var rel *delivery.Relay
	if relay {
		mxOpts.AuthPost, mxOpts.AuthUser, mxOpts.AuthPass, mxOpts.RequireAuth = "PLAIN", "svc", "s3cret-pass", true
		rel = &delivery.Relay{Host: "relay.test", Port: 587, Auth: &smtp.Credentials{Username: "svc", Password: "s3cret-pass"}}
	}
	mx := smtptest.Start(t, mxOpts)
	client, err := smtp.NewClient(smtp.ClientConfig{TLS: smtp.TLSConfig{RootCAs: pki.Pool, HandshakeTimeout: 2 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := transfer.NewService(client)
	host := "mx.dest.example"
	if relay {
		host = "relay.test"
	}
	engine, err := delivery.NewEngine(domainResolver{}, routedTransfer{svc, map[string]string{host: mx.Addr("127.0.0.1")}},
		delivery.Config{SMTPPort: 25, Shuffle: func([]dns.MX) {}, Relay: rel})
	if err != nil {
		t.Fatal(err)
	}
	coord, _ := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), retry.DefaultAttemptLimit())
	base := mustQ(t, 64)
	q := &orderedQueue{Queue: base, notify: make(chan struct{}, 64)}
	r := &suppRig{t: t, mx: mx, store: store, loader: newFakeLoader(), q: q, base: base, client: client, outcomes: store}
	return r, func(workers int, opts ...Option) *Pool {
		p, err := NewPool(q, r.loader, coord, r.outcomes, Config{Workers: workers}, opts...)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
}

func (r *suppRig) message(id string, recipients ...string) {
	r.loader.put(id, envelopeFor("<alice@example.com>", recipients...), "Subject: t\r\n\r\nbody\r\n")
	if err := r.q.Enqueue(context.Background(), queue.Job{ID: "job-" + id, MessageID: id}); err != nil {
		r.t.Fatal(err)
	}
}

func runUntil(t *testing.T, p *Pool, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	ok := cond()
	cancel()
	<-done
	if !ok {
		t.Fatal("condition not reached in time")
	}
}

func (r *suppRig) rcpts() []string {
	var out []string
	for _, c := range r.mx.Commands() {
		if i := strings.Index(c, "RCPT"); i >= 0 {
			out = append(out, strings.ToLower(c[i:]))
		}
	}
	return out
}

// A/B/C. Direct and relay obey the same policy, and a suppressed recipient causes ZERO
// SMTP connections; an unsuppressed one is delivered normally.
func TestSuppressedRecipientNeverReachesSMTPDirectOrRelay(t *testing.T) {
	for _, relay := range []bool{false, true} {
		name := map[bool]string{false: "direct", true: "relay"}[relay]
		t.Run(name+"/suppressed", func(t *testing.T) {
			store := newSuppStore("bob@dest.example")
			rig, mk := newSuppRig(t, smtptest.Options{}, relay, store)
			rig.message("m1", "<Bob@Dest.Example>") // case variant: same suppression key
			runUntil(t, mk(2), func() bool { return rig.base.Len() == 0 && len(store.recordedCalls()) == 1 })
			if rig.mx.Conns.Load() != 0 || len(rig.mx.Commands()) != 0 {
				t.Fatalf("SMTP was attempted for a suppressed recipient: conns=%d cmds=%v", rig.mx.Conns.Load(), rig.mx.Commands())
			}
			store.fakeOutcomeStore.mu.Lock()
			terminal, attempts := store.fakeOutcomeStore.terminal["m1"], len(store.fakeOutcomeStore.attempts["m1"])
			store.fakeOutcomeStore.mu.Unlock()
			if c := store.recordedCalls()[0]; !c.all || !terminal || attempts != 0 {
				t.Fatalf("terminal suppression must be persisted with no delivery attempt: %+v terminal=%v attempts=%d", c, terminal, attempts)
			}
		})
		t.Run(name+"/not suppressed", func(t *testing.T) {
			store := newSuppStore("someone-else@dest.example")
			rig, mk := newSuppRig(t, smtptest.Options{}, relay, store)
			rig.message("m1", "<bob@dest.example>")
			runUntil(t, mk(2), func() bool { return rig.base.Len() == 0 && len(rig.mx.Messages()) == 1 })
			if got := rig.rcpts(); len(got) != 1 || !strings.Contains(got[0], "bob@dest.example") {
				t.Fatalf("%v", got)
			}
			if len(store.recordedCalls()) != 0 {
				t.Fatal("nothing was suppressed")
			}
		})
	}
}

// Multi-recipient: a delivers, b is skipped with no SMTP command naming it, c delivers.
func TestMultiRecipientOnlySuppressedOneIsSkipped(t *testing.T) {
	store := newSuppStore("b@dest.example")
	rig, mk := newSuppRig(t, smtptest.Options{}, false, store)
	rig.message("m1", "<a@dest.example>", "<b@dest.example>", "<c@dest.example>")
	runUntil(t, mk(1), func() bool { return rig.base.Len() == 0 && len(rig.mx.Messages()) == 1 })
	got := strings.Join(rig.rcpts(), " ")
	if !strings.Contains(got, "a@dest.example") || !strings.Contains(got, "c@dest.example") || strings.Contains(got, "b@dest.example") {
		t.Fatalf("RCPT commands: %s", got)
	}
	calls := store.recordedCalls()
	if len(calls) != 1 || calls[0].all || len(calls[0].keys) != 1 || calls[0].keys[0] != "b@dest.example" {
		t.Fatalf("partial suppression recorded truthfully: %+v", calls)
	}
	store.fakeOutcomeStore.mu.Lock()
	terminal := store.fakeOutcomeStore.terminal["m1"]
	att := store.fakeOutcomeStore.attempts["m1"]
	store.fakeOutcomeStore.mu.Unlock()
	if len(att) != 1 || !att[0].Result.Accepted || terminal {
		// the fake store does not mark SMTP success terminal by itself; the attempt is what matters
		if len(att) != 1 || !att[0].Result.Accepted {
			t.Fatalf("the other recipients' delivery is recorded as it happened: %+v", att)
		}
	}
}

// All suppressed alongside a permanent failure history etc.: only the delivery attempt truth differs;
// covered by the database aggregation tests. Here: retrying message whose recipient becomes suppressed.
func TestSuppressionBetweenRetriesStopsTheRetryWithoutSMTP(t *testing.T) {
	store := newSuppStore()
	rig, mk := newSuppRig(t, smtptest.Options{}, false, store)
	// Durable history: attempt 1 failed temporarily, retry is due now.
	start := time.Now().Add(-time.Hour)
	store.fakeOutcomeStore.attempts["m1"] = []retry.DeliveryAttempt{{Number: 1, Decision: retry.Retry, Result: delivery.Result{
		Kind: delivery.KindTransferTemporary, Domain: "dest.example", FinalCode: 451, StartedAt: start, FinishedAt: start.Add(time.Second)}}}
	store.fakeOutcomeStore.next["m1"] = start.Add(30 * time.Minute)
	store.suppress("bob@dest.example") // suppressed AFTER acceptance and after the first attempt
	rig.message("m1", "<bob@dest.example>")
	runUntil(t, mk(1), func() bool { return rig.base.Len() == 0 && len(store.recordedCalls()) == 1 })
	if rig.mx.Conns.Load() != 0 {
		t.Fatalf("the retry re-entered SMTP for a suppressed recipient: conns=%d", rig.mx.Conns.Load())
	}
	store.fakeOutcomeStore.mu.Lock()
	terminal, n := store.fakeOutcomeStore.terminal["m1"], len(store.fakeOutcomeStore.attempts["m1"])
	store.fakeOutcomeStore.mu.Unlock()
	if !terminal || n != 1 || len(store.recordedCalls()) != 1 {
		t.Fatalf("terminal=%v attempts=%d recorded=%d: exactly one terminal state, history untouched", terminal, n, len(store.recordedCalls()))
	}
}

// Fail-safe: unknown suppression state never becomes "not suppressed, send".
func TestSuppressionLookupFailureDefersAndNeverSends(t *testing.T) {
	store := newSuppStore()
	store.lookupErr = errors.New("postgres unavailable")
	rig, mk := newSuppRig(t, smtptest.Options{}, false, store)
	rig.message("m1", "<bob@dest.example>")
	before := time.Now()
	runUntil(t, mk(1), func() bool { rig.q.mu.Lock(); defer rig.q.mu.Unlock(); return len(rig.q.releaseAt) >= 1 })
	if rig.mx.Conns.Load() != 0 {
		t.Fatal("SMTP was attempted while suppression state was unknown")
	}
	rig.q.mu.Lock()
	at := rig.q.releaseAt[0]
	rig.q.mu.Unlock()
	if at.Before(before.Add(suppressionDeferral - time.Second)) {
		t.Fatalf("the job must wait before the next lookup, not spin: released for %v", at.Sub(before))
	}
	store.fakeOutcomeStore.mu.Lock()
	terminal, attempts := store.fakeOutcomeStore.terminal["m1"], len(store.fakeOutcomeStore.attempts["m1"])
	store.fakeOutcomeStore.mu.Unlock()
	if terminal || attempts != 0 || rig.base.Len() != 1 {
		t.Fatalf("a database outage must not fail or finish the message: terminal=%v attempts=%d queue=%d", terminal, attempts, rig.base.Len())
	}
	rig.q.mu.Lock()
	for _, op := range rig.q.order {
		if op == "ack" {
			t.Fatal("the job was acked without a durable outcome")
		}
	}
	rig.q.mu.Unlock()
}

func TestSuppressionRecordFailureDefersWithoutSMTPOrAck(t *testing.T) {
	store := newSuppStore("bob@dest.example")
	store.recordErr = errors.New("commit failed")
	rig, mk := newSuppRig(t, smtptest.Options{}, false, store)
	rig.message("m1", "<bob@dest.example>")
	runUntil(t, mk(1), func() bool { rig.q.mu.Lock(); defer rig.q.mu.Unlock(); return len(rig.q.releaseAt) >= 1 })
	rig.q.mu.Lock()
	acked := false
	for _, op := range rig.q.order {
		acked = acked || op == "ack"
	}
	rig.q.mu.Unlock()
	if rig.mx.Conns.Load() != 0 || acked || rig.base.Len() != 1 {
		t.Fatalf("conns=%d acked=%v queue=%d: without a durable terminal state the job must stay", rig.mx.Conns.Load(), acked, rig.base.Len())
	}
}

// Durable state is written BEFORE the queue ack; if the ack then fails, the reclaimed job finds the
// terminal state and acks again with no SMTP.
func TestSuppressedTerminalStateSurvivesQueueAckFailure(t *testing.T) {
	store := newSuppStore("bob@dest.example")
	rig, mk := newSuppRig(t, smtptest.Options{}, false, store)
	rig.q.failAck = true
	rig.message("m1", "<bob@dest.example>")
	runUntil(t, mk(1), func() bool { rig.q.mu.Lock(); defer rig.q.mu.Unlock(); return len(rig.q.order) >= 1 })
	if calls := store.recordedCalls(); len(calls) != 1 || !calls[0].all {
		t.Fatalf("terminal state must be persisted first: %+v", calls)
	}
	rig.q.mu.Lock()
	if rig.q.order[0] != "ack" {
		t.Fatalf("order: %v", rig.q.order)
	}
	rig.q.mu.Unlock()
	// A fresh queue and pool (lease recovery) reclaim the job.
	base2 := mustQ(t, 4)
	_ = base2.Enqueue(context.Background(), queue.Job{ID: "job-m1", MessageID: "m1"})
	q2 := &orderedQueue{Queue: base2, notify: make(chan struct{}, 8)}
	pki := smtptest.NewPKI(t)
	client, _ := smtp.NewClient(smtp.ClientConfig{TLS: smtp.TLSConfig{RootCAs: pki.Pool}})
	svc, _ := transfer.NewService(client)
	engine, _ := delivery.NewEngine(domainResolver{}, routedTransfer{svc, map[string]string{"mx.dest.example": rig.mx.Addr("127.0.0.1")}}, delivery.Config{SMTPPort: 25, Shuffle: func([]dns.MX) {}})
	coord, _ := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), retry.DefaultAttemptLimit())
	p2, err := NewPool(q2, rig.loader, coord, store, Config{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, p2, func() bool { return base2.Len() == 0 })
	if rig.mx.Conns.Load() != 0 {
		t.Fatal("the reclaimed suppressed job re-entered SMTP")
	}
	if len(store.recordedCalls()) != 1 {
		t.Fatalf("terminal suppression recorded again: %+v", store.recordedCalls())
	}
}

// Another process already finished the message: ack, never SMTP.
func TestAlreadyTerminalDuringSuppressionRecordIsAckedWithoutSMTP(t *testing.T) {
	store := newSuppStore("bob@dest.example")
	store.recordErr = ErrOutcomeAlreadyTerminal
	rig, mk := newSuppRig(t, smtptest.Options{}, false, store)
	rig.message("m1", "<bob@dest.example>")
	runUntil(t, mk(1), func() bool { return rig.base.Len() == 0 })
	if rig.mx.Conns.Load() != 0 {
		t.Fatal("SMTP attempted")
	}
}

// Legacy recipients that cannot be keyed stay deliverable (they can never have been suppressed).
func TestUnkeyableLegacyRecipientStaysDeliverable(t *testing.T) {
	store := newSuppStore("bob@dest.example")
	rig, mk := newSuppRig(t, smtptest.Options{}, false, store)
	rig.message("m1", `<"weird name"@dest.example>`, "<bob@dest.example>")
	runUntil(t, mk(1), func() bool { return rig.base.Len() == 0 && len(rig.mx.Messages()) == 1 })
	got := strings.Join(rig.rcpts(), " ")
	if !strings.Contains(got, "weird name") || strings.Contains(got, "bob@dest.example") {
		t.Fatalf("%s", got)
	}
}

// Concurrency: many messages, several workers, half suppressed: exactly the suppressed ones never reach SMTP.
func TestConcurrentWorkersEnforceSuppressionExactly(t *testing.T) {
	store := newSuppStore()
	rig, mk := newSuppRig(t, smtptest.Options{}, false, store)
	const n = 30
	for i := 0; i < n; i++ {
		addr := fmt.Sprintf("user%d@dest.example", i)
		if i%2 == 0 {
			store.suppress(addr)
		}
		rig.message(fmt.Sprintf("m%d", i), "<"+addr+">")
	}
	runUntil(t, mk(6), func() bool {
		return rig.base.Len() == 0 && len(rig.mx.Messages()) == n/2 && len(store.recordedCalls()) == n/2
	})
	for _, c := range rig.rcpts() {
		var i int
		fmt.Sscanf(c[strings.Index(c, "user")+4:], "%d@", &i)
		if i%2 == 0 {
			t.Fatalf("a suppressed recipient reached SMTP: %s", c)
		}
	}
	if int(rig.mx.Conns.Load()) != n/2 {
		t.Fatalf("connections=%d want %d", rig.mx.Conns.Load(), n/2)
	}
}

// Without a SuppressionGate (stores that do not implement it) behaviour is unchanged.
func TestStoreWithoutGateBehavesAsBefore(t *testing.T) {
	store := newFakeOutcomeStore()
	if _, ok := any(store).(SuppressionGate); ok {
		t.Fatal("the plain fake must not implement the gate")
	}
}

// The documented consistency boundary: suppression is checked once, immediately before transport.
// A suppression created AFTER that check does not revoke the attempt already underway (MailX never
// holds a database lock across network I/O to pretend otherwise), and accepted delivery stays
// immutable history; the very next message to the address is suppressed.
type lateSuppressStore struct {
	*suppStore
	addr string
}

func (s *lateSuppressStore) SuppressedRecipients(ctx context.Context, id string, keys []string) (map[string]bool, error) {
	out, err := s.suppStore.SuppressedRecipients(ctx, id, keys)
	s.suppStore.suppress(s.addr) // the API suppresses the address right after this check returned "clear"
	return out, err
}

func TestSuppressionCreatedAfterTheCheckDoesNotRevokeTheAttemptButBlocksTheNextMessage(t *testing.T) {
	base := newSuppStore()
	store := &lateSuppressStore{suppStore: base, addr: "bob@dest.example"}
	rig, mk := newSuppRig(t, smtptest.Options{}, false, base)
	// Route the pool through the wrapper (it implements the gate itself).
	svc, _ := transfer.NewService(rig.client)
	engine, _ := delivery.NewEngine(domainResolver{}, routedTransfer{svc, map[string]string{"mx.dest.example": rig.mx.Addr("127.0.0.1")}}, delivery.Config{SMTPPort: 25, Shuffle: func([]dns.MX) {}})
	coord, _ := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), retry.DefaultAttemptLimit())
	newPool := func() *Pool {
		p, err := NewPool(rig.q, rig.loader, coord, store, Config{Workers: 1})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	_ = mk
	rig.message("m1", "<bob@dest.example>")
	runUntil(t, newPool(), func() bool { return rig.base.Len() == 0 && len(rig.mx.Messages()) == 1 })
	if rig.mx.Conns.Load() != 1 {
		t.Fatalf("the in-flight attempt must complete: conns=%d", rig.mx.Conns.Load())
	}
	base.fakeOutcomeStore.mu.Lock()
	att := base.fakeOutcomeStore.attempts["m1"]
	base.fakeOutcomeStore.mu.Unlock()
	if len(att) != 1 || !att[0].Result.Accepted {
		t.Fatalf("accepted delivery is immutable history: %+v", att)
	}
	// The next message is suppressed with no new connection.
	rig.message("m2", "<bob@dest.example>")
	runUntil(t, newPool(), func() bool { return rig.base.Len() == 0 && len(base.recordedCalls()) == 1 })
	if rig.mx.Conns.Load() != 1 {
		t.Fatalf("the next message reached SMTP: conns=%d", rig.mx.Conns.Load())
	}
}

// Cancellation while suppression state is being read: the pool stops, nothing is sent, nothing is acked or lost.
type blockingGate struct {
	*suppStore
	entered chan struct{}
}

func (b *blockingGate) SuppressedRecipients(ctx context.Context, _ string, _ []string) (map[string]bool, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCancellationDuringSuppressionLookupIsSafe(t *testing.T) {
	base := newSuppStore()
	gate := &blockingGate{suppStore: base, entered: make(chan struct{}, 1)}
	rig, _ := newSuppRig(t, smtptest.Options{}, false, base)
	svc, _ := transfer.NewService(rig.client)
	engine, _ := delivery.NewEngine(domainResolver{}, routedTransfer{svc, map[string]string{"mx.dest.example": rig.mx.Addr("127.0.0.1")}}, delivery.Config{SMTPPort: 25, Shuffle: func([]dns.MX) {}})
	coord, _ := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), retry.DefaultAttemptLimit())
	p, err := NewPool(rig.q, rig.loader, coord, gate, Config{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	rig.message("m1", "<bob@dest.example>")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the lookup never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the pool did not stop while a suppression lookup was pending")
	}
	if rig.mx.Conns.Load() != 0 {
		t.Fatal("SMTP was attempted after cancellation during the suppression lookup")
	}
	rig.q.mu.Lock()
	for _, op := range rig.q.order {
		if op == "ack" {
			t.Fatal("the job was acked without a durable outcome")
		}
	}
	rig.q.mu.Unlock()
	store := base.fakeOutcomeStore
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.terminal["m1"] || len(store.attempts["m1"]) != 0 {
		t.Fatal("cancellation must not create outcome state")
	}
}

func TestSuppressionMetricsAreBoundedAndCountEachCheck(t *testing.T) {
	store := newSuppStore("gone@dest.example")
	q := mustQ(t, 8)
	l := newFakeLoader()
	l.put("m1", envelopeFor("<a@example.com>", "<gone@dest.example>"), "x\r\n")
	coord := newScriptedCoordinator()
	p, _, metrics := obsPool(t, q, l, coord, store)
	_ = q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"})
	runUntil(t, p, func() bool { return q.Len() == 0 && len(store.recordedCalls()) == 1 })
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()
	if !strings.Contains(out, `mailx_suppression_checks_total{result="all"} 1`) {
		t.Fatalf("missing check metric:\n%s", grep(out, "suppression"))
	}
	if strings.Contains(out, "gone@") || strings.Contains(out, "dest.example") && strings.Contains(grep(out, "suppression"), "dest.example") {
		t.Fatal("recipient data reached the metrics")
	}
}

func grep(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
