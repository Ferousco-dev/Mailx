package bounce

import (
	"bufio"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func sampleDSN(t *testing.T) DSN {
	t.Helper()
	f := Failure{Class: FailurePermanentDelivery, SMTPCode: 550, RemoteMessage: "user unknown", Recipient: "<bob@example.com>"}
	dsn, err := NewDSN(f, []string{"<bob@example.com>"}, "mailx-a.local", "delivery-1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

func sampleOpts() MessageOptions {
	return MessageOptions{
		From:    "MailX Mailer Daemon <postmaster@mailx-a.local>",
		To:      "<alice@mailx-a.local>",
		Subject: "Delivery Status Notification (Failure)",
		Date:    time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC),
	}
}

// parseGenerated splits top-level headers from the body and returns the
// parsed message plus its multipart reader, exercising the same parsing
// path a receiving MTA/MUA would use — not string comparison.
func parseGenerated(t *testing.T, raw string) (*mail.Message, *multipart.Reader) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("failed to parse generated message: %v", err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("failed to parse Content-Type: %v", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/report") {
		t.Fatalf("expected multipart/report, got %q", mediaType)
	}
	if params["report-type"] != "delivery-status" {
		t.Fatalf("expected report-type=delivery-status, got %q", params["report-type"])
	}
	boundary := params["boundary"]
	if boundary == "" {
		t.Fatal("no boundary in Content-Type")
	}
	return msg, multipart.NewReader(msg.Body, boundary)
}

func TestGeneratePermanentFailureStructurallyValid(t *testing.T) {
	raw, err := Generate(sampleDSN(t), sampleOpts())
	if err != nil {
		t.Fatal(err)
	}
	msg, mr := parseGenerated(t, raw)
	if got := msg.Header.Get("From"); got != sampleOpts().From {
		t.Fatalf("From = %q", got)
	}
	if got := msg.Header.Get("Auto-Submitted"); got != "auto-replied" {
		t.Fatalf("Auto-Submitted = %q", got)
	}

	var parts []string
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		parts = append(parts, p.Header.Get("Content-Type"))
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts (no original headers supplied), got %d: %v", len(parts), parts)
	}
	if !strings.HasPrefix(parts[0], "text/plain") {
		t.Fatalf("part 0 should be text/plain, got %q", parts[0])
	}
	if !strings.HasPrefix(parts[1], "message/delivery-status") {
		t.Fatalf("part 1 should be message/delivery-status, got %q", parts[1])
	}
}

func TestGenerateDeliveryStatusPartContents(t *testing.T) {
	raw, err := Generate(sampleDSN(t), sampleOpts())
	if err != nil {
		t.Fatal(err)
	}
	_, mr := parseGenerated(t, raw)
	_, _ = mr.NextPart() // text/plain
	statusPart, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, statusPart)
	for _, want := range []string{
		"Reporting-MTA: dns;mailx-a.local",
		"Final-Recipient: rfc822;bob@example.com",
		"Action: failed",
		"Diagnostic-Code: smtp; 550 user unknown",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("delivery-status body missing %q:\n%s", want, body)
		}
	}
}

func TestGenerateWithOriginalHeaders(t *testing.T) {
	opts := sampleOpts()
	opts.OriginalHeaders = "From: alice@mailx-a.local\r\nTo: bob@example.com\r\nSubject: hi\r\n"
	raw, err := Generate(sampleDSN(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	_, mr := parseGenerated(t, raw)
	var parts []string
	var lastBody string
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		parts = append(parts, p.Header.Get("Content-Type"))
		lastBody = readAll(t, p)
	}
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d: %v", len(parts), parts)
	}
	if !strings.HasPrefix(parts[2], "text/rfc822-headers") {
		t.Fatalf("part 2 should be text/rfc822-headers, got %q", parts[2])
	}
	if !strings.Contains(lastBody, "Subject: hi") {
		t.Fatalf("original headers not preserved: %q", lastBody)
	}
	// Must NEVER contain a body — this is headers-only by design.
	if strings.Contains(lastBody, "body") {
		t.Fatalf("unexpectedly leaked body content: %q", lastBody)
	}
}

func TestGenerateMultipleRecipients(t *testing.T) {
	f := Failure{Class: FailureDNS}
	dsn, err := NewDSN(f, []string{"<a@example.com>", "<b@example.com>"}, "mailx-a.local", "d1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Generate(dsn, sampleOpts())
	if err != nil {
		t.Fatal(err)
	}
	_, mr := parseGenerated(t, raw)
	_, _ = mr.NextPart()
	statusPart, _ := mr.NextPart()
	body := readAll(t, statusPart)
	if strings.Count(body, "Final-Recipient:") != 2 {
		t.Fatalf("expected 2 Final-Recipient blocks:\n%s", body)
	}
}

func TestGenerateEnhancedStatusIncluded(t *testing.T) {
	es := EnhancedStatus{Class: 5, Subject: 1, Detail: 1}
	f := Failure{Class: FailurePermanentDelivery, SMTPCode: 550, EnhancedStatus: &es, Recipient: "<a@example.com>"}
	dsn, err := NewDSN(f, []string{"<a@example.com>"}, "mailx-a.local", "d1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Generate(dsn, sampleOpts())
	if err != nil {
		t.Fatal(err)
	}
	_, mr := parseGenerated(t, raw)
	_, _ = mr.NextPart()
	statusPart, _ := mr.NextPart()
	body := readAll(t, statusPart)
	if !strings.Contains(body, "Status: 5.1.1") {
		t.Fatalf("enhanced status missing:\n%s", body)
	}
}

func TestGenerateMissingEnhancedStatusFallback(t *testing.T) {
	f := Failure{Class: FailureDNS, Recipient: ""} // no EnhancedStatus set
	dsn, err := NewDSN(f, []string{"<a@example.com>"}, "mailx-a.local", "d1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Generate(dsn, sampleOpts())
	if err != nil {
		t.Fatal(err)
	}
	_, mr := parseGenerated(t, raw)
	_, _ = mr.NextPart()
	statusPart, _ := mr.NextPart()
	body := readAll(t, statusPart)
	if strings.Contains(body, "Status:") {
		t.Fatalf("must not fabricate a Status: line when no enhanced status is known:\n%s", body)
	}
}

func TestGenerateHostileDiagnosticCannotInjectHeader(t *testing.T) {
	f := Failure{
		Class:         FailurePermanentDelivery,
		SMTPCode:      550,
		RemoteMessage: "user unknown\r\nBcc: attacker@evil.test\r\nX-Injected: yes",
		Recipient:     "<a@example.com>",
	}
	dsn, err := NewDSN(f, []string{"<a@example.com>"}, "mailx-a.local", "d1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Generate(dsn, sampleOpts())
	if err != nil {
		t.Fatal(err)
	}
	// Must parse as exactly the header set we expect — an injected header
	// would either break parsing or add a header we didn't intend.
	msg, mr := parseGenerated(t, raw)
	if got := msg.Header.Get("Bcc"); got != "" {
		t.Fatalf("header injection succeeded: Bcc=%q", got)
	}
	if got := msg.Header.Get("X-Injected"); got != "" {
		t.Fatalf("header injection succeeded: X-Injected=%q", got)
	}
	_, _ = mr.NextPart()
	statusPart, _ := mr.NextPart()
	body := readAll(t, statusPart)
	if strings.Contains(body, "\r\nBcc:") || strings.Contains(body, "\r\nX-Injected:") {
		t.Fatalf("injected line survived into delivery-status body:\n%s", body)
	}
}

func TestGenerateHostileRecipientCannotInjectHeader(t *testing.T) {
	f := Failure{Class: FailureDNS}
	dsn, err := NewDSN(f, []string{"<a@example.com>\r\nX-Injected: yes>"}, "mailx-a.local", "d1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Generate(dsn, sampleOpts())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "\r\nX-Injected:") {
		t.Fatalf("recipient injection reached raw output:\n%s", raw)
	}
	// Must still parse cleanly.
	parseGenerated(t, raw)
}

func TestGenerateRejectsHostileFromHeader(t *testing.T) {
	opts := sampleOpts()
	opts.From = "attacker <a@example.test>\r\nBcc: victim@example.test"
	_, err := Generate(sampleDSN(t), opts)
	if err == nil {
		t.Fatal("expected error for CRLF in From header")
	}
}

func TestGenerateEmptyDSNRejected(t *testing.T) {
	_, err := Generate(DSN{}, sampleOpts())
	if err == nil {
		t.Fatal("expected ErrEmptyDSN")
	}
}

func TestGenerateCRLFThroughout(t *testing.T) {
	raw, err := Generate(sampleDSN(t), sampleOpts())
	if err != nil {
		t.Fatal(err)
	}
	// Every line ending must be CRLF; no bare LF.
	scanner := bufio.NewScanner(strings.NewReader(strings.ReplaceAll(raw, "\r\n", "\n")))
	_ = scanner
	if strings.Contains(raw, "\n") && strings.Count(raw, "\r\n") != strings.Count(raw, "\n") {
		t.Fatalf("bare LF found in generated output")
	}
}

func readAll(t *testing.T, r interface{ Read([]byte) (int, error) }) string {
	t.Helper()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return b.String()
}

// Adversarial audit: an unrecognized/zero-value FailureClass must never be
// treated as DSN-eligible, and a huge diagnostic string must not panic or
// be rejected outright — sanitizeText/Generate must remain bounded-safe.
func TestAuditUnknownFailureClassNeverEligible(t *testing.T) {
	if (Failure{Class: FailureClass(99)}).Eligible() {
		t.Fatal("unrecognized FailureClass must not be eligible")
	}
}

func TestAuditHugeDiagnosticDoesNotPanic(t *testing.T) {
	f := Failure{
		Class:         FailurePermanentDelivery,
		SMTPCode:      550,
		RemoteMessage: strings.Repeat("x", 1<<20), // 1 MiB
		Recipient:     "<a@example.com>",
	}
	dsn, err := NewDSN(f, []string{"<a@example.com>"}, "mailx-a.local", "d1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(dsn, sampleOpts()); err != nil {
		t.Fatalf("huge diagnostic must not fail generation: %v", err)
	}
}

func TestAuditEmptyStateNeverProducesFailure(t *testing.T) {
	// classify.go already refuses an empty state via ErrNoDeliveryAttempts;
	// this locks in that a Failure can never be silently zero-valued from
	// an empty lifecycle and treated as eligible.
	if (Failure{}).Eligible() {
		t.Fatal("zero-value Failure (FailureUnknown) must never be eligible")
	}
}
