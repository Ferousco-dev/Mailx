package retry

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/storage"
	"github.com/Ferousco-dev/mailx/internal/transfer"
)

// sequenceResolver is the deterministic DNS boundary in these integration
// tests. Each LookupMX call returns the next response, allowing separate
// retry-level delivery operations to see different local destinations.
type sequenceResolver struct {
	mu        sync.Mutex
	responses [][]dns.MX
	calls     int
}

func (r *sequenceResolver) LookupMX(ctx context.Context, domain string) ([]dns.MX, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls >= len(r.responses) {
		return nil, &dns.LookupError{Domain: domain, Kind: dns.KindResolverFailure, Err: errors.New("unexpected DNS lookup")}
	}
	response := append([]dns.MX(nil), r.responses[r.calls]...)
	r.calls++
	return response, nil
}

func (r *sequenceResolver) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestIntegrationRetryTemporaryThenSuccessfulDelivery(t *testing.T) {
	store, host, port := startRetryReceiver(t, 1)
	resolver := &sequenceResolver{responses: [][]dns.MX{
		{{Host: host, Preference: 10}}, // receiver responds 451 on its first save
		{{Host: host, Preference: 10}}, // receiver accepts and persists operation #2
	}}
	engine := newIntegrationDeliveryEngine(t, resolver, port)
	coordinator, err := NewCoordinator(engine, DefaultBackoffPolicy(), DefaultAttemptLimit())
	if err != nil {
		t.Fatal(err)
	}

	raw := integrationMessage()
	req := delivery.Request{
		Domain: "mailx-b.local",
		Envelope: mail.Envelope{
			MailFrom: "<bounce@mailx-a.local>",
			Recipients: []string{
				"<bob@mailx-b.local>",
				"<hidden-bcc@mailx-b.local>",
			},
		},
		Raw: raw,
	}
	state := &State{}
	now1 := time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)

	first, err := coordinator.Attempt(context.Background(), state, req, now1)
	if err != nil {
		t.Fatalf("first coordinator attempt: %v", err)
	}
	if first.DeliveryError == nil || first.Result.Kind != delivery.KindTransferTemporary {
		t.Fatalf("first delivery = result %+v, error %v", first.Result, first.DeliveryError)
	}
	if first.Status != StatusRetryable || state.Count() != 1 {
		t.Fatalf("first lifecycle = status %v, count %d", first.Status, state.Count())
	}
	if first.Schedule == nil || first.Schedule.Attempt != 1 || first.Schedule.Delay != 30*time.Minute {
		t.Fatalf("first schedule = %+v", first.Schedule)
	}
	if want := now1.Add(30 * time.Minute); !first.Schedule.NextRetryAt.Equal(want) {
		t.Fatalf("first NextRetryAt = %s, want %s", first.Schedule.NextRetryAt, want)
	}
	if len(first.Result.Attempts) != 1 || first.Result.Attempts[0].Transfer.AttemptID == "" {
		t.Fatalf("first delivery MX attempts = %+v", first.Result.Attempts)
	}
	if first.Result.FinalCode != 451 || first.Result.FailureStage != string(smtp.StageDataResponse) {
		t.Fatalf("first SMTP failure = code %d, stage %q", first.Result.FinalCode, first.Result.FailureStage)
	}
	if messages, listErr := store.List(); listErr != nil || len(messages) != 0 {
		t.Fatalf("receiver after temporary failure = %v, %v", messages, listErr)
	}

	now2 := first.Schedule.NextRetryAt
	second, err := coordinator.Attempt(context.Background(), state, req, now2)
	if err != nil {
		t.Fatalf("second coordinator attempt: %v", err)
	}
	if second.DeliveryError != nil || !second.Result.Accepted || second.Result.Kind != delivery.KindAccepted {
		t.Fatalf("second delivery = result %+v, error %v", second.Result, second.DeliveryError)
	}
	if second.Status != StatusSucceeded || second.Schedule != nil || state.Count() != 2 {
		t.Fatalf("second lifecycle = status %v, schedule %+v, count %d", second.Status, second.Schedule, state.Count())
	}
	if len(second.Result.Attempts) != 1 || !second.Result.Attempts[0].Transfer.Accepted {
		t.Fatalf("second delivery MX attempts = %+v", second.Result.Attempts)
	}

	history := state.History()
	if len(history) != 2 || history[0].Decision != Retry || history[1].Decision != TerminalSuccess {
		t.Fatalf("retry history = %+v", history)
	}
	if history[0].Result.DeliveryID == "" || history[1].Result.DeliveryID == "" || history[0].Result.DeliveryID == history[1].Result.DeliveryID {
		t.Fatalf("delivery operation IDs = %q, %q", history[0].Result.DeliveryID, history[1].Result.DeliveryID)
	}
	if len(history[0].Result.Attempts) != 1 || len(history[1].Result.Attempts) != 1 {
		t.Fatalf("retry operations must retain independent MX attempts: %+v", history)
	}

	assertIntegratedMessage(t, store, raw)
	if resolver.Calls() != 2 {
		t.Fatalf("DNS calls after two delivery operations = %d", resolver.Calls())
	}

	third, err := coordinator.Attempt(context.Background(), state, req, now2.Add(time.Hour))
	if !errors.Is(err, ErrNotRetryable) || third.Status != StatusSucceeded {
		t.Fatalf("third attempt = outcome %+v, error %v", third, err)
	}
	if resolver.Calls() != 2 {
		t.Fatalf("terminal retry invoked delivery/DNS; calls = %d", resolver.Calls())
	}
	messages, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("terminal retry duplicated accepted message; stored = %d", len(messages))
	}
}

func TestIntegrationRetryExhaustionBlocksFurtherDelivery(t *testing.T) {
	store, host, port := startRetryReceiver(t, 2)
	resolver := &sequenceResolver{responses: [][]dns.MX{
		{{Host: host, Preference: 10}},
		{{Host: host, Preference: 10}},
	}}
	engine := newIntegrationDeliveryEngine(t, resolver, port)
	coordinator, err := NewCoordinator(engine, DefaultBackoffPolicy(), AttemptLimit{MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	state := &State{}
	req := delivery.Request{
		Domain: "mailx-b.local",
		Envelope: mail.Envelope{
			MailFrom:   "<sender@mailx-a.local>",
			Recipients: []string{"<recipient@mailx-b.local>"},
		},
		Raw: "From: Sender <sender@example.com>\r\nTo: recipient@mailx-b.local\r\nSubject: exhaust\r\n\r\nbody\r\n",
	}
	now := time.Date(2026, time.September, 14, 14, 0, 0, 0, time.UTC)

	first, err := coordinator.Attempt(context.Background(), state, req, now)
	if err != nil || first.Status != StatusRetryable || first.Schedule == nil {
		t.Fatalf("first attempt = outcome %+v, error %v", first, err)
	}
	second, err := coordinator.Attempt(context.Background(), state, req, first.Schedule.NextRetryAt)
	if err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	if second.Status != StatusExhausted || second.Schedule != nil || state.Count() != 2 {
		t.Fatalf("exhausted lifecycle = outcome %+v, count %d", second, state.Count())
	}
	latest, ok := state.Latest()
	if !ok || latest.Number != 2 || latest.Decision != Retry {
		t.Fatalf("latest exhausted attempt = %+v, present %v", latest, ok)
	}

	third, err := coordinator.Attempt(context.Background(), state, req, now.Add(24*time.Hour))
	if !errors.Is(err, ErrRetryExhausted) || third.Status != StatusExhausted {
		t.Fatalf("third attempt = outcome %+v, error %v", third, err)
	}
	if resolver.Calls() != 2 {
		t.Fatalf("exhausted retry invoked delivery/DNS; calls = %d", resolver.Calls())
	}
	messages, err := store.List()
	if err != nil || len(messages) != 0 {
		t.Fatalf("unreachable deliveries persisted messages = %v, %v", messages, err)
	}
}

func startRetryReceiver(t *testing.T, temporaryFailures int) (*storage.FileStore, string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	var sinkMu sync.Mutex
	server, err := smtp.NewServer(smtp.DefaultConfig(), func(session smtp.Session, message mail.Message) error {
		sinkMu.Lock()
		if temporaryFailures > 0 {
			temporaryFailures--
			sinkMu.Unlock()
			return errors.New("controlled temporary storage failure")
		}
		sinkMu.Unlock()
		record, recordErr := storage.NewMessageRecord(session.Envelope, message)
		if recordErr != nil {
			return recordErr
		}
		return store.Save(record)
	})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		if serveErr := <-serveDone; serveErr != nil {
			t.Errorf("SMTP server shutdown: %v", serveErr)
		}
	})
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return store, host, port
}

func newIntegrationDeliveryEngine(t *testing.T, resolver delivery.Resolver, port int) *delivery.Engine {
	t.Helper()
	client, err := smtp.NewClient(smtp.ClientConfig{
		Identity:     "mailx-a.local",
		DialTimeout:  500 * time.Millisecond,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := transfer.NewService(client)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := delivery.NewEngine(resolver, service, delivery.Config{
		SMTPPort: port,
		Shuffle:  func([]dns.MX) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func integrationMessage() string {
	return "From: Alice Header <alice-header@example.com>\r\n" +
		"To: Bob <bob@mailx-b.local>\r\n" +
		"Subject: MailX retry integration\r\n" +
		"Message-ID: <retry-integration@mailx.test>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"mixed\"\r\n" +
		"\r\n" +
		"--mixed\r\n" +
		"Content-Type: multipart/alternative; boundary=\"alternative\"\r\n\r\n" +
		"--alternative\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n\r\n" +
		"Hello from retry.\r\n" +
		".leading dot survives\r\n" +
		"--alternative\r\n" +
		"Content-Type: text/html; charset=UTF-8\r\n\r\n" +
		"<p>Hello from <strong>retry</strong>.</p>\r\n" +
		"--alternative--\r\n" +
		"--mixed\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=\"retry.txt\"\r\n\r\n" +
		"cmV0cnkgYXR0YWNobWVudA==\r\n" +
		"--mixed--\r\n"
}

func assertIntegratedMessage(t *testing.T, store *storage.FileStore, wantRaw string) {
	t.Helper()
	messages, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("stored messages = %d, want 1", len(messages))
	}
	metadata := messages[0]
	if metadata.Envelope.MailFrom != "<bounce@mailx-a.local>" {
		t.Fatalf("stored envelope sender = %q", metadata.Envelope.MailFrom)
	}
	if len(metadata.Envelope.RcptTo) != 2 || metadata.Envelope.RcptTo[1] != "<hidden-bcc@mailx-b.local>" {
		t.Fatalf("stored envelope recipients = %v", metadata.Envelope.RcptTo)
	}
	if metadata.Message.From != "Alice Header <alice-header@example.com>" || metadata.Message.Subject != "MailX retry integration" {
		t.Fatalf("stored headers = %+v", metadata.Message)
	}
	if len(metadata.Message.Bcc) != 0 {
		t.Fatalf("envelope Bcc leaked into message headers: %v", metadata.Message.Bcc)
	}
	if len(metadata.Attachments) != 1 || metadata.Attachments[0].Filename != "retry.txt" {
		t.Fatalf("stored attachment metadata = %+v", metadata.Attachments)
	}

	loaded, err := store.Load(metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded.Raw) != wantRaw {
		t.Fatalf("stored raw message changed:\n got %q\nwant %q", string(loaded.Raw), wantRaw)
	}
	parsed, err := mail.ParseMessage(string(loaded.Raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parsed.TextBody, "Hello from retry.\r\n.leading dot survives") {
		t.Fatalf("stored text body = %q", parsed.TextBody)
	}
	if !strings.Contains(parsed.HTMLBody, "<p>Hello from <strong>retry</strong>.</p>") {
		t.Fatalf("stored HTML body = %q", parsed.HTMLBody)
	}
	if !strings.Contains(string(loaded.Raw), "\r\n.leading dot survives\r\n") {
		t.Fatalf("dot-stuffed line did not round-trip: %q", string(loaded.Raw))
	}

	attachment := metadata.Attachments[0]
	content, err := os.ReadFile(filepath.Join(store.MessagesDir(), metadata.ID, "attachments", attachment.StoredName))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "retry attachment" {
		t.Fatalf("stored attachment content = %q", string(content))
	}
}
