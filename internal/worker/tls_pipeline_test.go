package worker

import (
	"context"
	"net"
	"strings"
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

// domainResolver returns mx.<domain> for every domain.
type domainResolver struct{}

func (domainResolver) LookupMX(_ context.Context, domain string) ([]dns.MX, error) {
	return []dns.MX{{Host: "mx." + domain, Preference: 10}}, nil
}

// routedTransfer sends to a test server chosen by MX host, presenting the
// destination as "localhost:port" so certificate checks use the cert's SAN.
type routedTransfer struct {
	inner  *transfer.Service
	routes map[string]string // mx host -> "127.0.0.1:port"
}

func (r routedTransfer) Transfer(ctx context.Context, req transfer.Request) (transfer.Result, error) {
	host, _, _ := net.SplitHostPort(req.Destination)
	if addr, ok := r.routes[host]; ok {
		_, port, _ := net.SplitHostPort(addr)
		req.Destination = net.JoinHostPort("localhost", port)
	}
	return r.inner.Transfer(ctx, req)
}

// One recipient MX with broken TLS must not crash or wedge the worker pool:
// its job is retried later, while a healthy recipient in the same pool is
// delivered over verified TLS.
func TestWorkerSurvivesBrokenTLSPeerAndStillDeliversOthers(t *testing.T) {
	pki := smtptest.NewPKI(t)
	other := smtptest.NewPKI(t) // untrusted
	good := pki.Leaf(t, []string{"localhost"}, nil, time.Hour)
	bad := other.Leaf(t, []string{"localhost"}, nil, time.Hour)
	goodMX := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &good})
	badMX := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &bad})

	client, err := smtp.NewClient(smtp.ClientConfig{TLS: smtp.TLSConfig{Policy: smtp.TLSRequired, RootCAs: pki.Pool, HandshakeTimeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := transfer.NewService(client)
	rt := routedTransfer{svc, map[string]string{"mx.good.example": goodMX.Addr("127.0.0.1"), "mx.bad.example": badMX.Addr("127.0.0.1")}}
	engine, err := delivery.NewEngine(domainResolver{}, rt, delivery.Config{SMTPPort: 25, Shuffle: func([]dns.MX) {}})
	if err != nil {
		t.Fatal(err)
	}
	coord, err := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), retry.DefaultAttemptLimit())
	if err != nil {
		t.Fatal(err)
	}

	q := mustQ(t, 8)
	l := newFakeLoader()
	l.put("m-good", envelopeFor("<s@src.example>", "<r@good.example>"), "Subject: g\r\n\r\nbody\r\n")
	l.put("m-bad", envelopeFor("<s@src.example>", "<r@bad.example>"), "Subject: b\r\n\r\nbody\r\n")
	store := newFakeOutcomeStore()
	p, buf, _ := obsPool(t, q, l, coord, store)
	_ = q.Enqueue(context.Background(), queue.Job{ID: "j-bad", MessageID: "m-bad"})
	_ = q.Enqueue(context.Background(), queue.Job{ID: "j-good", MessageID: "m-good"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		store.mu.Lock()
		goodDone := store.terminal["m-good"]
		badRecorded := len(store.attempts["m-bad"]) == 1
		store.mu.Unlock()
		if goodDone && badRecorded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool did not process both jobs:\n%s", buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("pool exited because of a bad remote TLS peer")
	default:
	}
	cancel()
	<-done

	store.mu.Lock()
	badAttempt := store.attempts["m-bad"][0]
	terminalBad := store.terminal["m-bad"]
	store.mu.Unlock()
	if terminalBad || badAttempt.Decision != retry.Retry || badAttempt.Result.Kind != delivery.KindTransferTemporary {
		t.Fatalf("broken TLS must be a retryable temporary failure, got decision=%v kind=%s terminal=%v", badAttempt.Decision, badAttempt.Result.Kind, terminalBad)
	}
	if q.Len() != 1 {
		t.Fatalf("bad job must stay queued for retry and good job must be acked, Len=%d", q.Len())
	}
	for _, verb := range []string{"MAIL", "RCPT", "DATA"} {
		if badMX.SawAny(verb) {
			t.Fatalf("message traffic reached the peer that failed certificate verification: %v", badMX.Commands())
		}
	}
	if !goodMX.SawAny("DATA") || goodMX.SawPlain("MAIL") {
		t.Fatalf("good peer must receive the message over TLS only: %v", goodMX.Commands())
	}
	out := buf.String()
	if !strings.Contains(out, `"tls_outcome":"verify_failed"`) || !strings.Contains(out, `"tls_outcome":"established"`) {
		t.Fatalf("TLS outcomes missing from delivery logs:\n%s", out)
	}
	if strings.Contains(out, "x509") || strings.Contains(out, "bad.example") || strings.Contains(out, "localhost") {
		t.Fatalf("peer or recipient data leaked into logs:\n%s", out)
	}
}

// Successful DATA over TLS followed by a failing QUIT is still delivery truth:
// durable terminal success, acked, no second transmission.
func TestWorkerAcceptedOverTLSWithQuitFailureIsTerminalSuccess(t *testing.T) {
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, nil, time.Hour)
	mx := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &cert, QuitCloses: true})
	client, _ := smtp.NewClient(smtp.ClientConfig{TLS: smtp.TLSConfig{Policy: smtp.TLSRequired, RootCAs: pki.Pool}})
	svc, _ := transfer.NewService(client)
	rt := routedTransfer{svc, map[string]string{"mx.ok.example": mx.Addr("127.0.0.1")}}
	engine, _ := delivery.NewEngine(domainResolver{}, rt, delivery.Config{SMTPPort: 25, Shuffle: func([]dns.MX) {}})
	coord, _ := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), retry.DefaultAttemptLimit())
	q := mustQ(t, 4)
	l := newFakeLoader()
	l.put("m1", envelopeFor("<s@src.example>", "<r@ok.example>"), "Subject: x\r\n\r\nbody\r\n")
	store := newFakeOutcomeStore()
	p, _, _ := obsPool(t, q, l, coord, store)
	_ = q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	waitForQueueEmpty(t, q, 10*time.Second)
	cancel()
	<-done
	store.mu.Lock()
	defer store.mu.Unlock()
	if !store.terminal["m1"] || len(store.attempts["m1"]) != 1 || !store.attempts["m1"][0].Result.Accepted {
		t.Fatalf("accepted-with-QUIT-failure must be terminal success: %+v", store.attempts["m1"])
	}
	if mx.Conns.Load() != 1 {
		t.Fatalf("message was transmitted %d times", mx.Conns.Load())
	}
}
