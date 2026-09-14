package delivery

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/storage"
	"github.com/Ferousco-dev/mailx/internal/transfer"
)

// fixedResolver maps a domain to a fixed MX list; injected via NewEngine.
type fixedResolver map[string][]dns.MX

func (f fixedResolver) LookupMX(_ context.Context, domain string) ([]dns.MX, error) {
	mx, ok := f[strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))]
	if !ok {
		return nil, &dns.LookupError{Domain: domain, Kind: dns.KindNotFound}
	}
	return mx, nil
}

// startMailXB spawns a real MailX inbound server bound to 127.0.0.1:0 and
// returns (host, port, storageDir).
func startMailXB(t *testing.T) (string, int, string) {
	t.Helper()
	dir := t.TempDir()
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
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	return host, p, dir
}

func makeTransferService(t *testing.T) *transfer.Service {
	t.Helper()
	c, err := smtp.NewClient(smtp.ClientConfig{
		Identity: "mailx-a.local", DialTimeout: 2 * time.Second,
		ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := transfer.NewService(c)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// TestIntegrationMailXAtoMailXB proves the full stack:
// fake DNS → delivery.Engine → transfer.Service → smtp.Client → TCP →
// smtp.Server → parser → storage.FileStore.
// Also verifies raw preservation, dot-stuffing round-trip, MIME extraction,
// and envelope/Bcc separation.
func TestIntegrationMailXAtoMailXB(t *testing.T) {
	host, port, dir := startMailXB(t)
	svc := makeTransferService(t)
	resolver := fixedResolver{
		"mailx-b.local": {{Host: host, Preference: 10}},
	}
	engine, err := NewEngine(resolver, svc, Config{SMTPPort: port, Shuffle: noShuffle})
	if err != nil {
		t.Fatal(err)
	}

	raw := "From: Alice <alice@example.com>\r\n" +
		"To: Bob <bob@mailx-b.local>\r\n" +
		"Subject: engine round trip\r\n" +
		"Message-ID: <eng-1@mailx.test>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"B\"\r\n" +
		"\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"hello engine\r\n" +
		".leading dot line\r\n" +
		"--B\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=\"n.txt\"\r\n\r\n" +
		"aGVsbG8gYXR0YWNobWVudA==\r\n" +
		"--B--\r\n"

	res, err := engine.Deliver(context.Background(), Request{
		Domain: "mailx-b.local",
		Envelope: mail.Envelope{
			MailFrom:   "<bounce@mailx-a.local>",
			Recipients: []string{"<bob@mailx-b.local>", "<hidden-bcc@mailx-b.local>"},
		},
		Raw: raw,
	})
	if err != nil || !res.Accepted {
		t.Fatalf("delivery: %v %+v", err, res)
	}
	if res.DeliveryID == "" {
		t.Fatal("delivery id missing")
	}
	if len(res.Attempts) != 1 || !res.Attempts[0].Transfer.Accepted {
		t.Fatalf("attempts: %+v", res.Attempts)
	}
	if res.Attempts[0].Transfer.AttemptID == "" {
		t.Fatal("attempt id missing")
	}

	// Wait for persistence.
	deadline := time.Now().Add(2 * time.Second)
	var ids []string
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(filepath.Join(dir, "messages"))
		if len(entries) == 1 {
			for _, e := range entries {
				ids = append(ids, e.Name())
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(ids) != 1 {
		t.Fatalf("receiver did not persist exactly one message: %v", ids)
	}
	store, _ := storage.NewFileStore(dir)
	loaded, err := store.Load(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Metadata.Envelope.MailFrom != "<bounce@mailx-a.local>" {
		t.Fatalf("envelope MAIL FROM: %q", loaded.Metadata.Envelope.MailFrom)
	}
	if len(loaded.Metadata.Envelope.RcptTo) != 2 || loaded.Metadata.Envelope.RcptTo[1] != "<hidden-bcc@mailx-b.local>" {
		t.Fatalf("envelope recipients: %v", loaded.Metadata.Envelope.RcptTo)
	}
	if loaded.Metadata.Message.From != "Alice <alice@example.com>" {
		t.Fatalf("From header: %q", loaded.Metadata.Message.From)
	}
	if len(loaded.Metadata.Attachments) != 1 || loaded.Metadata.Attachments[0].Filename != "n.txt" {
		t.Fatalf("attachment metadata: %+v", loaded.Metadata.Attachments)
	}
	eml, err := os.ReadFile(filepath.Join(dir, "messages", ids[0], "message.eml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(eml), "\r\n.leading dot line\r\n") {
		t.Fatalf("dot-stuffing round-trip broken; eml=%q", string(eml))
	}
	att, err := os.ReadFile(filepath.Join(dir, "messages", ids[0], "attachments", "0001.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(att) != "hello attachment" {
		t.Fatalf("attachment content: %q", string(att))
	}
}

// TestIntegrationFallbackOnDeadFirstMX proves: first MX candidate is a dead
// TCP port; delivery falls back to the second (live MailX) and succeeds.
// Exactly one MX ends up receiving the message.
func TestIntegrationFallbackOnDeadFirstMX(t *testing.T) {
	// A closed listener gives us a definite "connection refused" host:port.
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadHost, deadPortStr, _ := net.SplitHostPort(deadLn.Addr().String())
	deadPort, _ := strconv.Atoi(deadPortStr)
	_ = deadLn.Close()

	liveHost, livePort, dir := startMailXB(t)
	svc := makeTransferService(t)

	// Both hosts share the same port config; encode the differing port in
	// the MX hostname? No — Config.SMTPPort is single. Instead, provide two
	// MX entries at the SAME port; make first a host that fails.
	// Simpler: use two Engines? No. Trick: pretend deadHost:deadPort by
	// aliasing — we cannot do that here. Solution: use a shim resolver
	// that returns the dead host and configure port to deadPort, then use
	// a second engine for the live path. That is not one delivery.
	//
	// Cleanest: run both listeners on the same explicit port. Use a Config
	// with SMTPPort = livePort; make the "dead" MX host resolve to a bogus
	// hostname MailX cannot dial. We can achieve that with 127.0.0.2:livePort
	// where nothing listens.
	// deadPort is unused in this simplified path.
	_ = deadHost
	_ = deadPort

	resolver := fixedResolver{
		"mailx-b.local": {
			{Host: "127.0.0.2", Preference: 10}, // nothing listens here
			{Host: liveHost, Preference: 20},
		},
	}
	engine, err := NewEngine(resolver, svc, Config{SMTPPort: livePort, Shuffle: noShuffle})
	if err != nil {
		t.Fatal(err)
	}
	// Small connect timeout so the failed dial does not slow the test.
	// We use the same smtp client, so we bound at dial via config already (2s).

	res, err := engine.Deliver(context.Background(), Request{
		Domain:   "mailx-b.local",
		Envelope: mail.Envelope{MailFrom: "<s@mailx-a.local>", Recipients: []string{"<r@mailx-b.local>"}},
		Raw:      "Subject: fallback\r\n\r\nbody\r\n",
	})
	if err != nil || !res.Accepted {
		t.Fatalf("fallback delivery failed: %v %+v", err, res)
	}
	if len(res.Attempts) != 2 {
		t.Fatalf("expected 2 attempts (fail, accept), got %d", len(res.Attempts))
	}
	if res.Attempts[1].MX.Host != liveHost {
		t.Fatalf("second attempt should be live host, got %+v", res.Attempts[1])
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "messages"))
	if len(entries) != 1 {
		t.Fatalf("expected exactly one persisted message, got %d", len(entries))
	}
}
