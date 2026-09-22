package worker

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/observability"
	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/retry"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/smtp/smtptest"
	"github.com/Ferousco-dev/mailx/internal/transfer"
)

const (
	relayUser = "MAILX_PRIVATE_USER_MARKER_pipe1"
	relayPass = "MAILX_PRIVATE_PASSWORD_MARKER_pipe2"
)

func enc(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// secretForms are every representation of the relay credentials a log, metric,
// error or result must never contain.
var secretForms = []string{relayUser, relayPass, enc(relayUser), enc(relayPass), enc("\x00" + relayUser + "\x00" + relayPass)}

func assertNoSecrets(t *testing.T, what, surface string) {
	t.Helper()
	for _, f := range secretForms {
		if strings.Contains(surface, f) {
			t.Fatalf("%s contains a credential form (%.8s...):\n%.600s", what, f, surface)
		}
	}
}

type relayRig struct {
	pool    *Pool
	queue   *queue.MemoryQueue
	store   *fakeOutcomeStore
	loader  *fakeLoader
	logs    *syncBuf
	metrics *observability.Metrics
	errs    *[]error
	errMu   *sync.Mutex
	mx      *smtptest.Server
}

// newRelayRig wires the REAL client, transfer, engine and coordinator to a fake
// TLS relay. The relay is reached as "relay.test:587" and routed to the server.
func newRelayRig(t *testing.T, opts smtptest.Options, readTimeout time.Duration, workers int) *relayRig {
	t.Helper()
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, nil, time.Hour)
	opts.Advertise, opts.Cert = true, &cert
	if opts.AuthUser == "" {
		opts.AuthUser, opts.AuthPass = relayUser, relayPass
	}
	if opts.AuthPost == "" {
		opts.AuthPost = "PLAIN LOGIN"
	}
	mx := smtptest.Start(t, opts)
	rig := &relayRig{mx: mx, store: newFakeOutcomeStore(), loader: newFakeLoader(), queue: mustQ(t, 128)}
	metrics, err := observability.NewMetrics(buildinfoForTest())
	if err != nil {
		t.Fatal(err)
	}
	rig.metrics = metrics
	client, err := smtp.NewClient(smtp.ClientConfig{ReadTimeout: readTimeout, AuthObserver: metrics,
		TLS: smtp.TLSConfig{RootCAs: pki.Pool, HandshakeTimeout: 10 * time.Second, Observer: metrics}})
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := transfer.NewService(client)
	rt := routedTransfer{svc, map[string]string{"relay.test": mx.Addr("127.0.0.1")}}
	engine, err := delivery.NewEngine(domainResolver{}, rt, delivery.Config{SMTPPort: 25, Shuffle: func([]dns.MX) {},
		Relay: &delivery.Relay{Host: "relay.test", Port: 587, Auth: &smtp.Credentials{Username: relayUser, Password: relayPass}}})
	if err != nil {
		t.Fatal(err)
	}
	coord, _ := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), retry.DefaultAttemptLimit())
	errFn, errs, mu := collectErrors()
	rig.errs, rig.errMu = errs, mu
	rig.logs = &syncBuf{}
	logger, _ := observability.NewLogger(rig.logs, "debug", "json")
	p, err := NewPool(rig.queue, rig.loader, coord, rig.store, Config{Workers: workers},
		WithLogger(logger), WithMetrics(metrics), WithOnError(*errFn))
	if err != nil {
		t.Fatal(err)
	}
	rig.pool = p
	return rig
}

func (r *relayRig) enqueue(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("m%d", i)
		r.loader.put(id, envelopeFor("<s@src.example>", "<r@dest.example>"), "Subject: x\r\n\r\nbody\r\n")
		if err := r.queue.Enqueue(context.Background(), queue.Job{ID: "j" + id, MessageID: id}); err != nil {
			t.Fatal(err)
		}
	}
}

// run processes until every message has one recorded attempt (or is terminal).
func (r *relayRig) run(t *testing.T, n int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.pool.Run(ctx); close(done) }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		r.store.mu.Lock()
		recorded := len(r.store.attempts)
		r.store.mu.Unlock()
		if recorded >= n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d attempts recorded\n%s", recorded, n, r.logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("pool exited")
	default:
	}
	cancel()
	<-done
}

func (r *relayRig) observable(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	r.errMu.Lock()
	defer r.errMu.Unlock()
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	return r.logs.String() + rec.Body.String() + fmt.Sprintf("%v|%+v", *r.errs, r.store.attempts)
}

// R. Across success and every failure mode, no credential form appears in logs,
// metrics, operational errors or persisted attempt results.
func TestRelayCredentialsNeverAppearInAnyObservableOutput(t *testing.T) {
	cases := map[string]struct {
		opts    smtptest.Options
		outcome string
	}{
		"success":         {smtptest.Options{}, "success"},
		"wrong password":  {smtptest.Options{AuthUser: relayUser, AuthPass: "different", AuthEcho: true}, "rejected"},
		"temporary":       {smtptest.Options{AuthReply: "454 4.7.0 busy " + relayPass}, "temporary"},
		"malformed reply": {smtptest.Options{AuthReply: "garbage " + relayUser}, "protocol_error"},
		"disconnect":      {smtptest.Options{AuthDrop: true}, "connection_lost"},
		"stall":           {smtptest.Options{AuthStall: true}, "timeout"},
	}
	for name, tc := range cases {
		rig := newRelayRig(t, tc.opts, 300*time.Millisecond, 1)
		rig.enqueue(t, 1)
		rig.run(t, 1)
		out := rig.observable(t)
		assertNoSecrets(t, name, out)
		if !strings.Contains(out, `"auth_outcome":"`+tc.outcome+`"`) || !strings.Contains(out, `"transport":"relay"`) {
			t.Fatalf("%s: auth facts missing from the delivery log:\n%s", name, out)
		}
		if !strings.Contains(out, `mailx_smtp_auth_attempts_total{mechanism=`) || !strings.Contains(out, `outcome="`+tc.outcome+`"`) {
			t.Fatalf("%s: auth metric missing", name)
		}
	}
}

// Wrong relay credentials must not hammer the relay: one attempt per job, then
// the existing backoff schedules the retry far in the future.
func TestBadRelayCredentialsCauseNoRetryStorm(t *testing.T) {
	rig := newRelayRig(t, smtptest.Options{AuthUser: "someone", AuthPass: "else"}, 400*time.Millisecond, 4)
	const jobs = 20
	rig.enqueue(t, jobs)
	rig.run(t, jobs)
	time.Sleep(600 * time.Millisecond) // a tight retry loop would keep hitting the relay
	if got := len(rig.mx.AuthAttempts()); got != jobs {
		t.Fatalf("relay saw %d AUTH attempts for %d jobs (retry storm)", got, jobs)
	}
	if got := rig.mx.Conns.Load(); got != jobs {
		t.Fatalf("relay saw %d connections for %d jobs", got, jobs)
	}
	rig.store.mu.Lock()
	defer rig.store.mu.Unlock()
	for id, attempts := range rig.store.attempts {
		if len(attempts) != 1 || attempts[0].Decision != retry.Retry || rig.store.terminal[id] {
			t.Fatalf("%s: want exactly one retryable attempt, got %d terminal=%v", id, len(attempts), rig.store.terminal[id])
		}
		// The front-loaded default's first delay is 5s (intentional — see
		// DefaultBackoffPolicy's doc); this only needs to prove the RETRY
		// SCHEDULE was used at all, not a tight zero-delay hammering loop.
		if next := rig.store.next[id]; time.Until(next) < 3*time.Second {
			t.Fatalf("%s: retry scheduled too soon (%v)", id, time.Until(next))
		}
	}
	if rig.queue.Len() != jobs {
		t.Fatalf("jobs must stay queued for retry, Len=%d", rig.queue.Len())
	}
}

// O. A relay that fails authentication never becomes a direct delivery.
func TestRelayAuthFailureDoesNotDeliverDirectly(t *testing.T) {
	rig := newRelayRig(t, smtptest.Options{AuthUser: "someone", AuthPass: "else"}, 400*time.Millisecond, 1)
	direct := smtptest.Start(t, smtptest.Options{}) // a plausible recipient MX
	rig.enqueue(t, 1)
	rig.run(t, 1)
	if direct.Conns.Load() != 0 {
		t.Fatal("relay failure must never fall back to a direct MX connection")
	}
	if rig.mx.SawAny("MAIL") || rig.mx.SawAny("DATA") {
		t.Fatal("no message may be submitted after failed authentication")
	}
}

// N (end to end). Direct delivery to an MX that offers AUTH never authenticates.
func TestDirectDeliveryToMXOfferingAuthNeverSendsAUTH(t *testing.T) {
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, nil, time.Hour)
	mx := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &cert, AuthPost: "PLAIN LOGIN", AuthUser: relayUser, AuthPass: relayPass})
	client, _ := smtp.NewClient(smtp.ClientConfig{TLS: smtp.TLSConfig{RootCAs: pki.Pool}})
	svc, _ := transfer.NewService(client)
	rt := routedTransfer{svc, map[string]string{"mx.dest.example": mx.Addr("127.0.0.1")}}
	engine, _ := delivery.NewEngine(domainResolver{}, rt, delivery.Config{SMTPPort: 25, Shuffle: func([]dns.MX) {}}) // no Relay
	coord, _ := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), retry.DefaultAttemptLimit())
	q := mustQ(t, 4)
	l := newFakeLoader()
	l.put("m1", envelopeFor("<s@src.example>", "<r@dest.example>"), "Subject: x\r\n\r\nbody\r\n")
	store := newFakeOutcomeStore()
	p, _, _ := obsPool(t, q, l, coord, store)
	_ = q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	waitForQueueEmpty(t, q, 10*time.Second)
	cancel()
	<-done
	if mx.SawAny("AUTH") || len(mx.AuthAttempts()) != 0 {
		t.Fatalf("direct delivery sent AUTH: %v", mx.Commands())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if !store.terminal["m1"] || store.attempts["m1"][0].Result.Transport != "direct" {
		t.Fatalf("direct delivery should have succeeded: %+v", store.attempts["m1"])
	}
}

// M (worker level). AUTH + DATA accepted + QUIT failure = terminal success, once.
func TestRelayAcceptedWithQuitFailureIsTerminalSuccess(t *testing.T) {
	rig := newRelayRig(t, smtptest.Options{QuitCloses: true, RequireAuth: true}, 2*time.Second, 1)
	rig.enqueue(t, 1)
	rig.run(t, 1)
	waitForQueueEmpty(t, rig.queue, 5*time.Second)
	rig.store.mu.Lock()
	defer rig.store.mu.Unlock()
	a := rig.store.attempts["m0"]
	if !rig.store.terminal["m0"] || len(a) != 1 || !a[0].Result.Accepted || rig.mx.Conns.Load() != 1 {
		t.Fatalf("attempts=%d terminal=%v conns=%d", len(a), rig.store.terminal["m0"], rig.mx.Conns.Load())
	}
}
