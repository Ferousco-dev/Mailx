package worker

import (
	"context"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"

	msgauth "github.com/emersion/go-msgauth/dkim"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/dkim"
	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/outbound"
	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/retry"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/smtp/smtptest"
	"github.com/Ferousco-dev/mailx/internal/transfer"
)

func signedMessage(t *testing.T, key *rsa.PrivateKey, domain, selector string) string {
	t.Helper()
	b, err := outbound.Build(outbound.Request{From: "alice@" + domain, To: []string{"bob@dest.example"}, Bcc: []string{"hidden@secret.example"},
		Subject: "signed over SMTP", Text: "hello\n.leading dot line\nend", MessageID: "<pipe1@mailx.local>", Date: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	out, err := dkim.Sign([]byte(b.Raw), dkim.Options{Domain: domain, Selector: selector, Key: key, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func verifyReceived(t *testing.T, received, domain, selector string, key *rsa.PrivateKey) error {
	t.Helper()
	pub, _ := dkim.PublicKeyBase64(&key.PublicKey)
	lookup := func(name string) ([]string, error) {
		if name == dkim.DNSName(selector, domain) {
			return []string{dkim.DNSValue(pub)}, nil
		}
		return nil, errors.New("no record")
	}
	res, err := msgauth.VerifyWithOptions(strings.NewReader(received), &msgauth.VerifyOptions{LookupTXT: lookup})
	if err != nil {
		return err
	}
	if len(res) != 1 {
		return errors.New("expected exactly one signature")
	}
	return res[0].Err
}

// run delivers one stored message through the given engine config and returns.
func deliverStored(t *testing.T, raw string, mx *smtptest.Server, host string, relay *delivery.Relay, pki *smtptest.PKI) *fakeOutcomeStore {
	t.Helper()
	client, err := smtp.NewClient(smtp.ClientConfig{TLS: smtp.TLSConfig{RootCAs: pki.Pool, HandshakeTimeout: 2 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := transfer.NewService(client)
	rt := routedTransfer{svc, map[string]string{host: mx.Addr("127.0.0.1")}}
	engine, err := delivery.NewEngine(domainResolver{}, rt, delivery.Config{SMTPPort: 25, Shuffle: func([]dns.MX) {}, Relay: relay})
	if err != nil {
		t.Fatal(err)
	}
	coord, _ := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), retry.DefaultAttemptLimit())
	q := mustQ(t, 4)
	l := newFakeLoader()
	l.put("m1", envelopeFor("<alice@example.com>", "<bob@dest.example>"), raw)
	store := newFakeOutcomeStore()
	p, _, _ := obsPool(t, q, l, coord, store)
	_ = q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	waitForQueueEmpty(t, q, 10*time.Second)
	cancel()
	<-done
	return store
}

// P/Q: the message transmitted over SMTP (direct MX and authenticated relay) is
// byte-identical to the stored signed message and verifies cryptographically.
func TestDirectAndRelayDeliverBytesThatVerifyAndEqualTheStoredMessage(t *testing.T) {
	key, err := dkim.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	const domain, selector = "example.com", "mx20260921ab12"
	raw := signedMessage(t, key, domain, selector)
	if strings.Contains(strings.ToLower(raw), "hidden@secret") || strings.Contains(strings.ToLower(raw), "bcc:") {
		t.Fatal("Bcc leaked into the signed message")
	}
	for name, mode := range map[string]string{"direct": "mx.dest.example", "relay": "relay.test"} {
		pki := smtptest.NewPKI(t)
		cert := pki.Leaf(t, []string{"localhost"}, nil, time.Hour)
		opts := smtptest.Options{Advertise: true, Cert: &cert, Capture: true}
		var relay *delivery.Relay
		if mode == "relay.test" {
			opts.AuthPost, opts.AuthUser, opts.AuthPass, opts.RequireAuth = "PLAIN", "svc", "s3cret-pass", true
			relay = &delivery.Relay{Host: "relay.test", Port: 587, Auth: &smtp.Credentials{Username: "svc", Password: "s3cret-pass"}}
		}
		mx := smtptest.Start(t, opts)
		store := deliverStored(t, raw, mx, mode, relay, pki)
		got := mx.Messages()
		if len(got) != 1 {
			t.Fatalf("%s: %d messages received", name, len(got))
		}
		if got[0] != raw {
			t.Fatalf("%s: transmitted bytes differ from the stored signed message (signing boundary violated)", name)
		}
		if err := verifyReceived(t, got[0], domain, selector, key); err != nil {
			t.Fatalf("%s: receiver-side DKIM verification failed: %v", name, err)
		}
		store.mu.Lock()
		ok := store.terminal["m1"] && store.attempts["m1"][0].Result.Accepted
		store.mu.Unlock()
		if !ok {
			t.Fatalf("%s: delivery not recorded as accepted", name)
		}
	}
}

// Y. Signed delivery accepted by DATA, then QUIT fails: still accepted, sent once.
func TestSignedAcceptedDeliverySurvivesQuitFailureAndIsNotRetransmitted(t *testing.T) {
	key, _ := dkim.GenerateKey()
	raw := signedMessage(t, key, "example.com", "mx20260921ab12")
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, nil, time.Hour)
	mx := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &cert, Capture: true, QuitCloses: true})
	store := deliverStored(t, raw, mx, "mx.dest.example", nil, pki)
	store.mu.Lock()
	defer store.mu.Unlock()
	a := store.attempts["m1"]
	if len(a) != 1 || !a[0].Result.Accepted || !store.terminal["m1"] || len(mx.Messages()) != 1 || mx.Conns.Load() != 1 {
		t.Fatalf("attempts=%d received=%d conns=%d", len(a), len(mx.Messages()), mx.Conns.Load())
	}
}
