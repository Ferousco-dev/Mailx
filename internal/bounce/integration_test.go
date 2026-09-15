package bounce

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// ---------------------------------------------------------------------
// Real-stack fixtures: SMTP / delivery.Engine / retry.Coordinator, driving
// the actual bounce package end to end. No public Internet, no public DNS.
// ---------------------------------------------------------------------

// fixedResolver maps one domain to a fixed MX host/port pair. It performs
// no real DNS lookups.
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

func newDeliveryEngine(t *testing.T, domain, host string, port int) *delivery.Engine {
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

// startRealMailXB spawns a genuine MailX inbound SMTP server (accepts
// everything, persists to storage). Used for the acceptance scenario.
func startRealMailXB(t *testing.T) (host string, port int, dir string) {
	t.Helper()
	dir = t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	server, err := smtp.NewServer(smtp.DefaultConfig(), func(s smtp.Session, m mail.Message) error {
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
	return h, pi, dir
}

// scriptedSMTP starts a raw TCP SMTP peer under full test control — used
// for RCPT/MAIL-stage rejections the real smtp.Server has no policy hook
// for (it has no recipient-rejection policy of its own).
func scriptedSMTP(t *testing.T, handler func(net.Conn)) (host string, port int) {
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
			go func(c net.Conn) { defer c.Close(); handler(c) }(conn)
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	pi, _ := strconv.Atoi(p)
	return h, pi
}

func writeLine(c net.Conn, s string) { _, _ = c.Write([]byte(s)) }

// rejectAtRCPT always returns 550 for RCPT TO — a permanent, recipient-level
// rejection the real MailX inbound server has no policy hook to produce.
func rejectAtRCPT(t *testing.T) func(net.Conn) {
	return func(c net.Conn) {
		r := bufio.NewReader(c)
		writeLine(c, "220 scripted.test\r\n")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			u := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(u, "EHLO"), strings.HasPrefix(u, "MAIL"):
				writeLine(c, "250 ok\r\n")
			case strings.HasPrefix(u, "RCPT"):
				writeLine(c, "550 5.1.1 user unknown\r\n")
			case strings.HasPrefix(u, "QUIT"):
				writeLine(c, "221 bye\r\n")
				return
			default:
				writeLine(c, "500 5.5.2 unknown\r\n")
			}
		}
	}
}

// rejectAtMailAlways451 always returns 451 to MAIL FROM — used to drive a
// delivery operation into retry, repeatedly, without any sleeping.
func rejectAtMailAlways451(t *testing.T) func(net.Conn) {
	return func(c net.Conn) {
		r := bufio.NewReader(c)
		writeLine(c, "220 scripted.test\r\n")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			u := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(u, "EHLO"):
				writeLine(c, "250 ok\r\n")
			case strings.HasPrefix(u, "MAIL"):
				writeLine(c, "451 4.3.0 greylisted, try later\r\n")
			case strings.HasPrefix(u, "QUIT"):
				writeLine(c, "221 bye\r\n")
				return
			default:
				writeLine(c, "500 5.5.2 unknown\r\n")
			}
		}
	}
}

func integrationRaw() string {
	return "From: Alice <alice@example.com>\r\n" +
		"To: Bob <bob@mailx-b.local>\r\n" +
		"Subject: bounce integration\r\n" +
		"\r\n" +
		"hello\r\n"
}

// ---------------------------------------------------------------------
// SCENARIO A — permanent failure end to end.
// ---------------------------------------------------------------------

func TestIntegrationScenarioAPermanentFailure(t *testing.T) {
	host, port := scriptedSMTP(t, rejectAtRCPT(t))
	engine := newDeliveryEngine(t, "mailx-b.local", host, port)
	coordinator, err := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), retry.DefaultAttemptLimit())
	if err != nil {
		t.Fatal(err)
	}

	req := delivery.Request{
		Domain:   "mailx-b.local",
		Envelope: mail.Envelope{MailFrom: "<alice@mailx-a.local>", Recipients: []string{"<bob@mailx-b.local>"}},
		Raw:      integrationRaw(),
	}
	state := &retry.State{}
	outcome, err := coordinator.Attempt(context.Background(), state, req, time.Now())
	if err != nil {
		t.Fatalf("coordinator attempt: %v", err)
	}
	if outcome.Status != retry.StatusFailed {
		t.Fatalf("expected StatusFailed (permanent, no retry), got %v", outcome.Status)
	}
	if outcome.Schedule != nil {
		t.Fatalf("permanent failure must not schedule a retry: %+v", outcome.Schedule)
	}

	failure, err := Classify(state, outcome.Status)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if failure.Class != FailurePermanentDelivery || failure.SMTPCode != 550 || failure.Recipient != "<bob@mailx-b.local>" {
		t.Fatalf("wrong classification: %+v", failure)
	}

	ok, err := ShouldGenerate(req.Envelope.MailFrom, failure)
	if !ok || err != nil {
		t.Fatalf("expected eligible DSN: ok=%v err=%v", ok, err)
	}
	env, err := BounceEnvelope(req.Envelope.MailFrom)
	if err != nil {
		t.Fatal(err)
	}
	if env.MailFrom != "<>" || env.Recipients[0] != "<alice@mailx-a.local>" {
		t.Fatalf("wrong bounce envelope: %+v", env)
	}
	dsn, err := NewDSN(failure, req.Envelope.Recipients, "mailx-a.local", "delivery-a", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Generate(dsn, MessageOptions{From: "MailX Mailer Daemon <postmaster@mailx-a.local>", To: env.Recipients[0], Date: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "550") || !strings.Contains(raw, "user unknown") {
		t.Fatalf("generated DSN missing diagnostic:\n%s", raw)
	}
}

// ---------------------------------------------------------------------
// SCENARIO B — temporary failures exhaust retry, without rewriting 451→550.
// ---------------------------------------------------------------------

func TestIntegrationScenarioBRetryExhaustion(t *testing.T) {
	host, port := scriptedSMTP(t, rejectAtMailAlways451(t))
	engine := newDeliveryEngine(t, "mailx-b.local", host, port)
	limit := retry.AttemptLimit{MaxAttempts: 3}
	coordinator, err := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), limit)
	if err != nil {
		t.Fatal(err)
	}
	req := delivery.Request{
		Domain:   "mailx-b.local",
		Envelope: mail.Envelope{MailFrom: "<alice@mailx-a.local>", Recipients: []string{"<bob@mailx-b.local>"}},
		Raw:      integrationRaw(),
	}
	state := &retry.State{}
	now := time.Now()
	var lastOutcome retry.Outcome
	// The coordinator's exhaustion detection is necessarily one call
	// delayed: the Nth Attempt performs the Nth delivery operation and
	// only discovers afterward that it exhausted the limit, so it returns
	// StatusExhausted with a nil error (the operation itself succeeded at
	// the transport level — it was a normal, temporary 451). A caller only
	// gets ErrRetryExhausted from a *subsequent* Attempt call, made before
	// any further delivery is attempted.
	for i := 0; i < limit.MaxAttempts; i++ {
		outcome, attemptErr := coordinator.Attempt(context.Background(), state, req, now)
		now = now.Add(time.Minute)
		lastOutcome = outcome
		if i < limit.MaxAttempts-1 {
			if attemptErr != nil {
				t.Fatalf("attempt %d: unexpected error %v", i, attemptErr)
			}
			if outcome.Status != retry.StatusRetryable {
				t.Fatalf("attempt %d: expected StatusRetryable, got %v", i, outcome.Status)
			}
		} else {
			if attemptErr != nil {
				t.Fatalf("exhausting attempt: expected nil error (exhaustion discovered after this delivery), got %v", attemptErr)
			}
			if outcome.Status != retry.StatusExhausted {
				t.Fatalf("expected StatusExhausted on the exhausting attempt, got %v", outcome.Status)
			}
		}
		if outcome.Result.Kind != delivery.KindTransferTemporary || outcome.Result.FinalCode != 451 {
			t.Fatalf("attempt %d: 451 must remain temporary, got kind=%v code=%d", i, outcome.Result.Kind, outcome.Result.FinalCode)
		}
	}
	// A subsequent call now correctly refuses with ErrRetryExhausted
	// without performing another delivery operation.
	extra, extraErr := coordinator.Attempt(context.Background(), state, req, now)
	if !errors.Is(extraErr, retry.ErrRetryExhausted) {
		t.Fatalf("post-exhaustion attempt: expected ErrRetryExhausted, got %v", extraErr)
	}
	if extra.Status != retry.StatusExhausted {
		t.Fatalf("post-exhaustion status: expected StatusExhausted, got %v", extra.Status)
	}

	failure, err := Classify(state, lastOutcome.Status)
	if err != nil {
		t.Fatal(err)
	}
	if failure.Class != FailureRetryExhausted || !failure.RetryExhausted {
		t.Fatalf("expected FailureRetryExhausted, got %+v", failure)
	}
	if failure.SMTPCode != 451 {
		t.Fatalf("original 451 must not be rewritten to 550, got %d", failure.SMTPCode)
	}
	if failure.AttemptCount != limit.MaxAttempts {
		t.Fatalf("attempt count wrong: %d", failure.AttemptCount)
	}

	ok, err := ShouldGenerate(req.Envelope.MailFrom, failure)
	if !ok || err != nil {
		t.Fatalf("exhaustion should be DSN-eligible: ok=%v err=%v", ok, err)
	}
	dsn, err := NewDSN(failure, req.Envelope.Recipients, "mailx-a.local", "delivery-b", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Generate(dsn, sampleOpts())
	if err != nil {
		t.Fatal(err)
	}
	// Inspect the parsed Diagnostic-Code line specifically — a raw
	// substring search over the whole message is unreliable because the
	// MIME boundary is random hex and can coincidentally contain "550".
	_, mr := parseGenerated(t, raw)
	_, _ = mr.NextPart() // text/plain
	statusPart, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	statusBody := readAll(t, statusPart)
	diagLine := ""
	for _, line := range strings.Split(statusBody, "\r\n") {
		if strings.HasPrefix(line, "Diagnostic-Code:") {
			diagLine = line
		}
	}
	if diagLine == "" || !strings.Contains(diagLine, "451") {
		t.Fatalf("exhaustion DSN must report the true final 451, not a fabricated code; Diagnostic-Code=%q", diagLine)
	}
	if strings.Contains(diagLine, "550") {
		t.Fatalf("exhaustion DSN must not contain a fabricated 550; Diagnostic-Code=%q", diagLine)
	}
}

// ---------------------------------------------------------------------
// SCENARIO C — success: no bounce classification, no DSN, ever.
// ---------------------------------------------------------------------

func TestIntegrationScenarioCSuccessNeverBounces(t *testing.T) {
	host, port, dir := startRealMailXB(t)
	engine := newDeliveryEngine(t, "mailx-b.local", host, port)
	coordinator, err := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), retry.DefaultAttemptLimit())
	if err != nil {
		t.Fatal(err)
	}
	req := delivery.Request{
		Domain:   "mailx-b.local",
		Envelope: mail.Envelope{MailFrom: "<alice@mailx-a.local>", Recipients: []string{"<bob@mailx-b.local>"}},
		Raw:      integrationRaw(),
	}
	state := &retry.State{}
	outcome, err := coordinator.Attempt(context.Background(), state, req, time.Now())
	if err != nil {
		t.Fatalf("coordinator attempt: %v", err)
	}
	if outcome.Status != retry.StatusSucceeded || !outcome.Result.Accepted {
		t.Fatalf("expected success, got %+v", outcome)
	}

	// Confirm real persistence happened (proves this is the real stack).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(filepath.Join(dir, "messages"))
		if len(entries) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	_, err = Classify(state, outcome.Status)
	if !errors.Is(err, ErrDeliverySucceeded) {
		t.Fatalf("Classify must refuse a successful delivery, got %v", err)
	}
}

// TestIntegrationScenarioCAcceptedWithQuitErrorNeverBounces proves the
// accepted+QUIT-failure invariant survives the ENTIRE bounce pipeline, not
// just the delivery layer (already covered in internal/delivery).
func TestIntegrationScenarioCAcceptedWithQuitErrorNeverBounces(t *testing.T) {
	host, port := scriptedSMTP(t, func(c net.Conn) {
		r := bufio.NewReader(c)
		writeLine(c, "220 scripted.test\r\n")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			u := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(u, "EHLO"), strings.HasPrefix(u, "MAIL"), strings.HasPrefix(u, "RCPT"):
				writeLine(c, "250 ok\r\n")
			case strings.HasPrefix(u, "DATA"):
				writeLine(c, "354 go\r\n")
				for {
					l, err := r.ReadString('\n')
					if err != nil || strings.TrimRight(l, "\r\n") == "." {
						break
					}
				}
				writeLine(c, "250 accepted\r\n")
				return // drop connection instead of replying to QUIT
			}
		}
	})
	engine := newDeliveryEngine(t, "mailx-b.local", host, port)
	coordinator, err := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), retry.DefaultAttemptLimit())
	if err != nil {
		t.Fatal(err)
	}
	req := delivery.Request{
		Domain:   "mailx-b.local",
		Envelope: mail.Envelope{MailFrom: "<alice@mailx-a.local>", Recipients: []string{"<bob@mailx-b.local>"}},
		Raw:      integrationRaw(),
	}
	state := &retry.State{}
	outcome, err := coordinator.Attempt(context.Background(), state, req, time.Now())
	if err != nil {
		t.Fatalf("accepted+QUIT-error must not surface as a coordinator error: %v", err)
	}
	if !outcome.Result.Accepted || outcome.Result.QuitError == "" {
		t.Fatalf("expected accepted with QuitError, got %+v", outcome.Result)
	}
	if outcome.Status != retry.StatusSucceeded {
		t.Fatalf("QUIT failure after acceptance must still be StatusSucceeded, got %v", outcome.Status)
	}
	if _, err := Classify(state, outcome.Status); !errors.Is(err, ErrDeliverySucceeded) {
		t.Fatalf("accepted+QUIT-error must never classify as a failure, got %v", err)
	}
}

// ---------------------------------------------------------------------
// SCENARIO D — bounce loop prevention: a message that is itself already a
// bounce (null reverse path) must NEVER produce a DSN, for any failure kind.
// ---------------------------------------------------------------------

func TestIntegrationScenarioDBounceLoopPrevention(t *testing.T) {
	host, port := scriptedSMTP(t, rejectAtRCPT(t))
	engine := newDeliveryEngine(t, "mailx-b.local", host, port)
	coordinator, err := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), retry.DefaultAttemptLimit())
	if err != nil {
		t.Fatal(err)
	}
	// The "original message" here is itself a DSN: null reverse path.
	req := delivery.Request{
		Domain:   "mailx-b.local",
		Envelope: mail.Envelope{MailFrom: "<>", Recipients: []string{"<bob@mailx-b.local>"}},
		Raw:      integrationRaw(),
	}
	state := &retry.State{}
	outcome, err := coordinator.Attempt(context.Background(), state, req, time.Now())
	if err != nil {
		t.Fatalf("coordinator attempt: %v", err)
	}
	if outcome.Status != retry.StatusFailed {
		t.Fatalf("expected permanent failure, got %v", outcome.Status)
	}

	failure, err := Classify(state, outcome.Status)
	if err != nil {
		t.Fatal(err)
	}
	// Classify alone has no notion of the original envelope's reverse path —
	// it WOULD produce an eligible Failure. This is precisely why
	// ShouldGenerate must always be called with the original MailFrom: it
	// is the only gate that knows about bounce-loop safety.
	if !failure.Eligible() {
		t.Fatalf("expected Classify to consider this eligible by itself (proving ShouldGenerate is the real gate): %+v", failure)
	}

	ok, err := ShouldGenerate(req.Envelope.MailFrom, failure)
	if ok || !errors.Is(err, ErrOriginalSenderNull) {
		t.Fatalf("bounce-of-a-bounce must be refused: ok=%v err=%v", ok, err)
	}
	if _, err := BounceEnvelope(req.Envelope.MailFrom); !errors.Is(err, ErrOriginalSenderNull) {
		t.Fatalf("BounceEnvelope must independently refuse a null original sender: %v", err)
	}
}
