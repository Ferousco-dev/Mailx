package smtp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/smtp/smtptest"
)

var localhostIPs = []net.IP{net.ParseIP("127.0.0.1")}

func tlsClient(t testing.TB, pki *smtptest.PKI, policy TLSPolicy, obs TLSObserver) *Client {
	t.Helper()
	cfg := testClientConfig()
	cfg.TLS = TLSConfig{Policy: policy, RootCAs: pki.Pool, HandshakeTimeout: 2 * time.Second, Observer: obs}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func request(addr string) DeliveryRequest {
	return DeliveryRequest{
		Address:  addr,
		Envelope: mail.Envelope{MailFrom: "<a@example.com>", Recipients: []string{"<b@example.net>"}},
		Raw:      "Subject: t\r\n\r\nbody\r\n",
	}
}

type recObserver struct {
	mu     sync.Mutex
	events []string
}

func (o *recObserver) TLSResult(policy, outcome, version string) {
	o.mu.Lock()
	o.events = append(o.events, policy+"/"+outcome+"/"+version)
	o.mu.Unlock()
}

func (o *recObserver) last() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.events) == 0 {
		return ""
	}
	return o.events[len(o.events)-1]
}

type panicTLSObserver struct{}

func (panicTLSObserver) TLSResult(string, string, string) { panic("observer bug") }

func stageOf(t *testing.T, err error) *DeliveryError {
	t.Helper()
	var de *DeliveryError
	if !errors.As(err, &de) {
		t.Fatalf("expected *DeliveryError, got %T %v", err, err)
	}
	return de
}

// A. STARTTLS happy path, including the exact command order the server saw.
func TestStartTLSHappyPathCommandOrder(t *testing.T) {
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	mx := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &cert})
	obs := &recObserver{}
	res, err := tlsClient(t, pki, TLSOpportunistic, obs).Send(context.Background(), request(mx.Addr("localhost")))
	if err != nil || !res.Accepted {
		t.Fatalf("send: %+v %v", res, err)
	}
	want := []string{"plain:EHLO", "plain:STARTTLS", "tls:EHLO", "tls:MAIL", "tls:RCPT", "tls:DATA", "tls:QUIT"}
	got := mx.Commands()
	if len(got) != len(want) {
		t.Fatalf("commands = %v", got)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Fatalf("command %d = %q, want prefix %q (all: %v)", i, got[i], want[i], got)
		}
	}
	if !res.TLS.Established || !res.TLS.Advertised || !res.TLS.Attempted || res.TLS.Outcome != OutcomeEstablished {
		t.Fatalf("TLS info = %+v", res.TLS)
	}
	if res.TLS.Version != "1.2" && res.TLS.Version != "1.3" {
		t.Fatalf("negotiated version %q", res.TLS.Version)
	}
	if obs.last() != "opportunistic/established/"+res.TLS.Version {
		t.Fatalf("observer = %q", obs.last())
	}
}

// B. Opportunistic policy and no STARTTLS: plaintext delivery, recorded as such.
func TestOpportunisticWithoutSTARTTLSDeliversPlaintext(t *testing.T) {
	pki := smtptest.NewPKI(t)
	mx := smtptest.Start(t, smtptest.Options{})
	res, err := tlsClient(t, pki, TLSOpportunistic, nil).Send(context.Background(), request(mx.Addr("localhost")))
	if err != nil || !res.Accepted {
		t.Fatalf("send: %+v %v", res, err)
	}
	if res.TLS.Outcome != OutcomeNotOffered || res.TLS.Established || res.TLS.Attempted || !mx.SawPlain("MAIL") {
		t.Fatalf("TLS=%+v plainMAIL=%v", res.TLS, mx.SawPlain("MAIL"))
	}
}

// C. Required TLS and no STARTTLS: nothing but EHLO/QUIT-class traffic in plaintext.
func TestRequiredPolicyWithoutSTARTTLSSendsNoMessage(t *testing.T) {
	pki := smtptest.NewPKI(t)
	mx := smtptest.Start(t, smtptest.Options{})
	obs := &recObserver{}
	res, err := tlsClient(t, pki, TLSRequired, obs).Send(context.Background(), request(mx.Addr("localhost")))
	de := stageOf(t, err)
	if res.Accepted || de.Stage != StageStartTLS || !de.Temporary || res.TLS.Outcome != OutcomeRequiredNoTLS {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	for _, verb := range []string{"MAIL", "RCPT", "DATA"} {
		if mx.SawAny(verb) {
			t.Fatalf("%s reached a server that lacks STARTTLS under required policy: %v", verb, mx.Commands())
		}
	}
	if obs.last() != "required/required_unavailable/none" {
		t.Fatalf("observer = %q", obs.last())
	}
}

// D. STARTTLS advertised but rejected: temporary, no plaintext continuation.
func TestStartTLSRejectedIsTemporaryAndDoesNotContinuePlaintext(t *testing.T) {
	pki := smtptest.NewPKI(t)
	for _, reply := range []string{"454 4.7.0 TLS not available", "554 5.7.0 no", "501 5.5.4 syntax"} {
		mx := smtptest.Start(t, smtptest.Options{Advertise: true, StartTLSReply: reply})
		res, err := tlsClient(t, pki, TLSOpportunistic, nil).Send(context.Background(), request(mx.Addr("localhost")))
		de := stageOf(t, err)
		if de.Stage != StageStartTLS || !de.Temporary || res.TLS.Outcome != OutcomeRejected {
			t.Fatalf("%q: stage=%s temp=%v outcome=%s", reply, de.Stage, de.Temporary, res.TLS.Outcome)
		}
		if mx.SawAny("MAIL") || mx.Conns.Load() != 1 {
			t.Fatalf("%q: continued after refusal: %v conns=%d", reply, mx.Commands(), mx.Conns.Load())
		}
	}
}

// E. Handshake failure modes: never a plaintext retry, never MAIL FROM.
func TestHandshakeFailureNeverFallsBackToPlaintext(t *testing.T) {
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	for name, mode := range map[string]smtptest.HandshakeMode{"close": smtptest.HSClose, "garbage": smtptest.HSGarbage} {
		mx := smtptest.Start(t, smtptest.Options{Advertise: true, Handshake: mode, Cert: &cert})
		res, err := tlsClient(t, pki, TLSOpportunistic, nil).Send(context.Background(), request(mx.Addr("localhost")))
		de := stageOf(t, err)
		if res.Accepted || de.Stage != StageTLS || !de.Temporary {
			t.Fatalf("%s: res=%+v err=%v", name, res, err)
		}
		if mx.SawPlain("MAIL") || mx.SawPlain("DATA") || mx.Conns.Load() != 1 {
			t.Fatalf("%s: plaintext fallback detected: %v conns=%d", name, mx.Commands(), mx.Conns.Load())
		}
		var tf *TLSFailure
		if !errors.As(err, &tf) || strings.Contains(err.Error(), "tls: ") || strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("%s: error must carry a bounded TLS category, got %v", name, err)
		}
	}
}

// F. A peer that stalls the handshake is cut off by the bounded timeout.
func TestHandshakeStallIsBoundedAndLeaksNothing(t *testing.T) {
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	mx := smtptest.Start(t, smtptest.Options{Advertise: true, Handshake: smtptest.HSStall, Cert: &cert})
	cfg := testClientConfig()
	cfg.TLS = TLSConfig{RootCAs: pki.Pool, HandshakeTimeout: 300 * time.Millisecond}
	c, _ := NewClient(cfg)
	before := runtime.NumGoroutine()
	start := time.Now()
	res, err := c.Send(context.Background(), request(mx.Addr("localhost")))
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("stalled handshake took %v", elapsed)
	}
	de := stageOf(t, err)
	if de.Stage != StageTLS || res.TLS.Outcome != OutcomeHandshakeTimeout {
		t.Fatalf("stage=%s outcome=%s err=%v", de.Stage, res.TLS.Outcome, err)
	}
	waitGoroutines(t, before)
}

func waitGoroutines(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > baseline+3 {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines leaked: %d now vs %d before", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// G. Certificate verification: untrusted CA, wrong name, expired. No plaintext.
func TestCertificateVerificationFailuresAreClassifiedAndFailClosed(t *testing.T) {
	pki := smtptest.NewPKI(t)
	other := smtptest.NewPKI(t) // a CA the client does not trust
	cases := map[string]struct {
		cert *smtptest.PKI
		dns  []string
		ttl  time.Duration
		host string
	}{
		"untrusted issuer":  {other, []string{"localhost"}, time.Hour, "localhost"},
		"hostname mismatch": {pki, []string{"other.example"}, time.Hour, "localhost"},
		"ip not in san":     {pki, []string{"localhost"}, time.Hour, "127.0.0.1"},
		"expired":           {pki, []string{"localhost"}, -30 * time.Minute, "localhost"},
	}
	for name, tc := range cases {
		cert := tc.cert.Leaf(t, tc.dns, nil, tc.ttl)
		mx := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &cert})
		res, err := tlsClient(t, pki, TLSOpportunistic, nil).Send(context.Background(), request(mx.Addr(tc.host)))
		de := stageOf(t, err)
		if de.Stage != StageTLS || !de.Temporary || res.TLS.Outcome != OutcomeVerifyFailed || res.Accepted {
			t.Fatalf("%s: stage=%s outcome=%s err=%v", name, de.Stage, res.TLS.Outcome, err)
		}
		if mx.SawPlain("MAIL") || mx.Conns.Load() != 1 {
			t.Fatalf("%s: message data reached an unverified peer: %v", name, mx.Commands())
		}
		if strings.Contains(err.Error(), "other.example") || strings.Contains(err.Error(), "x509") {
			t.Fatalf("%s: peer-controlled certificate text leaked into the error: %v", name, err)
		}
	}
}

// H. Capabilities from before the handshake are discarded; only post-TLS ones remain.
func TestCapabilitiesAreResetAfterSTARTTLS(t *testing.T) {
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	mx := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &cert,
		PreCaps: []string{"SIZE 1000", "8BITMIME"}, PostCaps: []string{"SMTPUTF8", "PIPELINING"}})
	cfg := testClientConfig()
	cfg.TLS = TLSConfig{RootCAs: pki.Pool, HandshakeTimeout: 2 * time.Second}
	cfg, _ = cfg.normalized()
	conn, err := net.Dial("tcp", mx.Addr("localhost"))
	if err != nil {
		t.Fatal(err)
	}
	s := &clientSession{raw: conn, conn: conn, config: cfg, serverName: "localhost", reader: newTestReader(conn, cfg)}
	defer s.close()
	if _, _, _, err := s.readReply(); err != nil {
		t.Fatal(err)
	}
	if derr := s.hello(); derr != nil {
		t.Fatal(derr)
	}
	if !s.caps.has("SIZE") || !s.caps.has("STARTTLS") || s.caps.has("SMTPUTF8") {
		t.Fatalf("pre-TLS caps wrong: %v", s.caps)
	}
	if derr := s.secure(context.Background()); derr != nil {
		t.Fatal(derr)
	}
	if s.caps.has("SIZE") || s.caps.has("8BITMIME") || s.caps.has("STARTTLS") {
		t.Fatalf("stale pre-TLS capabilities survived: %v", s.caps)
	}
	if !s.caps.has("SMTPUTF8") || !s.caps.has("PIPELINING") {
		t.Fatalf("post-TLS capabilities missing: %v", s.caps)
	}
}

// I. Post-TLS EHLO failure: bounded failure, no HELO fallback, no MAIL.
func TestPostTLSEHLOFailure(t *testing.T) {
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	mx := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &cert, PostEHLOReply: "502 5.5.1 no EHLO here"})
	res, err := tlsClient(t, pki, TLSOpportunistic, nil).Send(context.Background(), request(mx.Addr("localhost")))
	de := stageOf(t, err)
	if de.Stage != StageEHLOTLS || !de.Temporary || res.TLS.Outcome != OutcomeEHLOFailed {
		t.Fatalf("stage=%s outcome=%s err=%v", de.Stage, res.TLS.Outcome, err)
	}
	if mx.SawAny("HELO") || mx.SawAny("MAIL") {
		t.Fatalf("HELO fallback or MAIL after failed post-TLS EHLO: %v", mx.Commands())
	}
}

// Legacy EHLO refusal still falls back to HELO, and then cannot use TLS.
func TestHELOFallbackHasNoSTARTTLS(t *testing.T) {
	pki := smtptest.NewPKI(t)
	mx := smtptest.Start(t, smtptest.Options{HeloOnly: true})
	res, err := tlsClient(t, pki, TLSOpportunistic, nil).Send(context.Background(), request(mx.Addr("localhost")))
	if err != nil || !res.Accepted || res.TLS.Outcome != OutcomeNotOffered {
		t.Fatalf("opportunistic HELO peer: %+v %v", res, err)
	}
	mx2 := smtptest.Start(t, smtptest.Options{HeloOnly: true})
	_, err = tlsClient(t, pki, TLSRequired, nil).Send(context.Background(), request(mx2.Addr("localhost")))
	if de := stageOf(t, err); de.Stage != StageStartTLS || mx2.SawAny("MAIL") {
		t.Fatalf("required policy over HELO: stage=%s cmds=%v", de.Stage, mx2.Commands())
	}
}

// J. Final DATA acceptance over TLS is delivery truth even if QUIT fails.
func TestAcceptedOverTLSSurvivesQuitFailure(t *testing.T) {
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	mx := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &cert, QuitCloses: true})
	res, err := tlsClient(t, pki, TLSRequired, nil).Send(context.Background(), request(mx.Addr("localhost")))
	if err != nil || !res.Accepted || res.FinalCode != 250 {
		t.Fatalf("accepted delivery must not become an error: %+v %v", res, err)
	}
	if res.QuitError == nil || !res.TLS.Established {
		t.Fatalf("QUIT failure should be recorded on an established TLS session: %+v", res)
	}
	if mx.Conns.Load() != 1 {
		t.Fatal("must not reconnect after acceptance")
	}
}

// K. Cancellation during the handshake returns promptly.
func TestCancellationDuringHandshake(t *testing.T) {
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	mx := smtptest.Start(t, smtptest.Options{Advertise: true, Handshake: smtptest.HSStall, Cert: &cert})
	cfg := testClientConfig()
	cfg.TLS = TLSConfig{RootCAs: pki.Pool, HandshakeTimeout: time.Minute}
	c, _ := NewClient(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	before := runtime.NumGoroutine()
	start := time.Now()
	res, err := c.Send(ctx, request(mx.Addr("localhost")))
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancellation took %v", elapsed)
	}
	if err == nil || res.TLS.Outcome != OutcomeCanceled {
		t.Fatalf("outcome=%s err=%v", res.TLS.Outcome, err)
	}
	waitGoroutines(t, before)
}

// STARTTLS reply followed by extra plaintext bytes is command injection.
func TestDataInjectedAfterSTARTTLSReplyIsRejected(t *testing.T) {
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	mx := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &cert, InjectAfter: "250 injected\r\n"})
	res, err := tlsClient(t, pki, TLSOpportunistic, nil).Send(context.Background(), request(mx.Addr("localhost")))
	de := stageOf(t, err)
	if de.Stage != StageStartTLS || res.TLS.Outcome != OutcomeProtocolViolation || res.TLS.Established {
		t.Fatalf("stage=%s outcome=%s err=%v", de.Stage, res.TLS.Outcome, err)
	}
}

// A panicking observer must never change the delivery result.
func TestTLSObserverPanicDoesNotAffectDelivery(t *testing.T) {
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	mx := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &cert})
	res, err := tlsClient(t, pki, TLSOpportunistic, panicTLSObserver{}).Send(context.Background(), request(mx.Addr("localhost")))
	if err != nil || !res.Accepted {
		t.Fatalf("%+v %v", res, err)
	}
}

// L + stress: concurrent sessions against mixed peers; run under -race.
func TestConcurrentTLSDeliveriesMixedPeers(t *testing.T) {
	pki := smtptest.NewPKI(t)
	other := smtptest.NewPKI(t)
	good := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	bad := other.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	okMX := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &good})
	badCert := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &bad})
	closer := smtptest.Start(t, smtptest.Options{Advertise: true, Handshake: smtptest.HSClose, Cert: &good})
	stall := smtptest.Start(t, smtptest.Options{Advertise: true, Handshake: smtptest.HSStall, Cert: &good})
	plain := smtptest.Start(t, smtptest.Options{})
	cfg := testClientConfig()
	cfg.TLS = TLSConfig{RootCAs: pki.Pool, HandshakeTimeout: 2 * time.Second} // generous for slow CI; the stall peer still ends at the timeout
	c, _ := NewClient(cfg)

	before := runtime.NumGoroutine()
	type outcome struct {
		kind     string
		accepted bool
		err      error
	}
	const rounds = 12
	results := make(chan outcome, rounds*5)
	var wg sync.WaitGroup
	for range rounds {
		for kind, mx := range map[string]*smtptest.Server{"ok": okMX, "badcert": badCert, "close": closer, "stall": stall, "plain": plain} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := c.Send(context.Background(), request(mx.Addr("localhost")))
				results <- outcome{kind, res.Accepted, err}
			}()
		}
	}
	wg.Wait()
	close(results)
	for r := range results {
		wantOK := r.kind == "ok" || r.kind == "plain"
		if r.accepted != wantOK || (r.err == nil) != wantOK {
			t.Fatalf("%s: accepted=%v err=%v", r.kind, r.accepted, r.err)
		}
	}
	for name, mx := range map[string]*smtptest.Server{"badcert": badCert, "close": closer, "stall": stall} {
		if mx.SawAny("MAIL") {
			t.Fatalf("%s peer saw message traffic", name)
		}
	}
	waitGoroutines(t, before)
}

func TestParseTLSPolicyAndConfigValidation(t *testing.T) {
	for in, want := range map[string]TLSPolicy{"": TLSOpportunistic, "Opportunistic": TLSOpportunistic, " required ": TLSRequired} {
		if got, err := ParseTLSPolicy(in); err != nil || got != want {
			t.Fatalf("%q -> %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"none", "optional", "true", "off"} {
		if _, err := ParseTLSPolicy(bad); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
	if _, err := NewClient(ClientConfig{TLS: TLSConfig{HandshakeTimeout: -1}}); err == nil {
		t.Fatal("negative handshake timeout must be rejected")
	}
	if _, err := NewClient(ClientConfig{TLS: TLSConfig{Policy: 9}}); err == nil {
		t.Fatal("unknown policy must be rejected")
	}
	c, _ := ClientConfig{}.normalized()
	if c.TLS.Policy != TLSOpportunistic || c.TLS.HandshakeTimeout <= 0 || c.TLS.RootCAs != nil {
		t.Fatalf("default TLS config = %+v", c.TLS)
	}
}

func TestParseCapabilities(t *testing.T) {
	caps := parseCapabilities("mx.test greets you\nstarttls\nSIZE 10240000\n8BITMIME")
	if !caps.has("STARTTLS") || caps["SIZE"] != "10240000" || !caps.has("8bitmime") || caps.has("mx.test") {
		t.Fatalf("%v", caps)
	}
	if len(parseCapabilities("")) != 0 || len(parseCapabilities("only greeting")) != 0 {
		t.Fatal("greeting line is not a capability")
	}
}

// M. Peer-controlled certificate names never appear in errors, TLS facts or
// observer events (which feed logs and metrics).
func TestTLSFailureCarriesNoPeerControlledText(t *testing.T) {
	pki := smtptest.NewPKI(t)
	const marker = "PRIVATE-CERT-MARKER"
	cert := pki.Leaf(t, []string{strings.ToLower(marker) + ".example"}, nil, time.Hour)
	mx := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &cert})
	obs := &recObserver{}
	res, err := tlsClient(t, pki, TLSRequired, obs).Send(context.Background(), request(mx.Addr("localhost")))
	if err == nil {
		t.Fatal("hostname mismatch must fail")
	}
	surface := err.Error() + fmt.Sprintf("%+v", res.TLS) + obs.last()
	if strings.Contains(strings.ToUpper(surface), marker) || strings.Contains(surface, "x509") {
		t.Fatalf("peer-controlled text leaked: %s", surface)
	}
}

func newTestReader(c net.Conn, cfg ClientConfig) *bufio.Reader {
	return bufio.NewReaderSize(c, cfg.MaxReplyLineBytes*2)
}

// Connection loss right after the handshake (before the post-TLS EHLO reply).
func TestConnectionLostAfterHandshake(t *testing.T) {
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	mx := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &cert, DropAfterTLS: true})
	res, err := tlsClient(t, pki, TLSRequired, nil).Send(context.Background(), request(mx.Addr("localhost")))
	de := stageOf(t, err)
	if de.Stage != StageEHLOTLS || !de.Temporary || res.TLS.Outcome != OutcomeConnectionLost || mx.SawAny("MAIL") {
		t.Fatalf("stage=%s temp=%v outcome=%s cmds=%v", de.Stage, de.Temporary, res.TLS.Outcome, mx.Commands())
	}
}
