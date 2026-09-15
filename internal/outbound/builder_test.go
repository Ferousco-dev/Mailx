package outbound

import (
	"mime"
	"strings"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
)

func TestBuildTextOnly(t *testing.T) {
	built, err := Build(Request{
		From: "Alice <alice@example.com>", To: []string{"bob@example.com"},
		Subject: "hi", Text: "hello there", Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(built.Raw, "From: \"Alice\" <alice@example.com>\r\n") {
		t.Fatalf("missing From header:\n%s", built.Raw)
	}
	if !strings.Contains(built.Raw, "To: <bob@example.com>\r\n") {
		t.Fatalf("missing To header:\n%s", built.Raw)
	}
	if got := built.Envelope; len(got) != 1 || got[0] != "<bob@example.com>" {
		t.Fatalf("unexpected envelope: %v", got)
	}
	if built.From != "<alice@example.com>" {
		t.Fatalf("unexpected envelope from: %s", built.From)
	}

	parsed, err := mail.ParseMessage(built.Raw)
	if err != nil {
		t.Fatalf("built message must be parseable by MailX's own inbound parser: %v", err)
	}
	if parsed.TextBody != "hello there" {
		t.Fatalf("round-trip text mismatch: %q", parsed.TextBody)
	}
}

func TestBuildRejectsMissingBody(t *testing.T) {
	_, err := Build(Request{From: "a@example.com", To: []string{"b@example.com"}})
	if err != ErrEmptyBody {
		t.Fatalf("expected ErrEmptyBody, got %v", err)
	}
}

func TestBuildRejectsNoRecipients(t *testing.T) {
	_, err := Build(Request{From: "a@example.com", Text: "x"})
	if err != ErrNoRecipient {
		t.Fatalf("expected ErrNoRecipient, got %v", err)
	}
}

func TestBuildRejectsInvalidAddress(t *testing.T) {
	_, err := Build(Request{From: "not-an-address", To: []string{"b@example.com"}, Text: "x"})
	if err == nil {
		t.Fatal("expected an error for an invalid From address")
	}
}

func TestBuildRejectsCRLFInjectionInSubject(t *testing.T) {
	_, err := Build(Request{
		From: "a@example.com", To: []string{"b@example.com"}, Text: "x",
		Subject: "hi\r\nBcc: attacker@evil.com",
	})
	if err == nil {
		t.Fatal("expected CRLF injection in subject to be rejected")
	}
}

func TestBuildRejectsCRLFInjectionInFrom(t *testing.T) {
	_, err := Build(Request{
		From: "a@example.com\r\nBcc: attacker@evil.com", To: []string{"b@example.com"}, Text: "x",
	})
	if err == nil {
		t.Fatal("expected CRLF injection in From to be rejected")
	}
}

func TestBuildRejectsCRLFInjectionInRecipient(t *testing.T) {
	_, err := Build(Request{
		From: "a@example.com", To: []string{"b@example.com\r\nBcc: attacker@evil.com"}, Text: "x",
	})
	if err == nil {
		t.Fatal("expected CRLF injection in a recipient to be rejected")
	}
}

// CRITICAL: Bcc must reach the envelope (so the message is actually
// delivered to them) but must NEVER appear in the visible built headers.
func TestBuildBccInEnvelopeNotInHeaders(t *testing.T) {
	built, err := Build(Request{
		From: "a@example.com", To: []string{"b@example.com"}, Bcc: []string{"secret@example.com"},
		Text: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(built.Raw, "secret@example.com") {
		t.Fatalf("Bcc address leaked into message headers/body:\n%s", built.Raw)
	}
	if strings.Contains(strings.ToLower(built.Raw), "bcc:") {
		t.Fatalf("a Bcc header line must never be written:\n%s", built.Raw)
	}
	var found bool
	for _, e := range built.Envelope {
		if e == "<secret@example.com>" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Bcc recipient missing from envelope: %v", built.Envelope)
	}
}

func TestBuildTextAndHTMLMultipartAlternative(t *testing.T) {
	built, err := Build(Request{
		From: "a@example.com", To: []string{"b@example.com"},
		Text: "plain body", HTML: "<p>html body</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(built.Raw, "multipart/alternative") {
		t.Fatalf("expected multipart/alternative, got:\n%s", built.Raw)
	}
	parsed, err := mail.ParseMessage(built.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.TextBody != "plain body" {
		t.Fatalf("text part mismatch: %q", parsed.TextBody)
	}
	if parsed.HTMLBody != "<p>html body</p>" {
		t.Fatalf("html part mismatch: %q", parsed.HTMLBody)
	}
}

func TestBuildUnicodeSubjectIsRFC2047Encoded(t *testing.T) {
	built, err := Build(Request{
		From: "a@example.com", To: []string{"b@example.com"}, Text: "x",
		Subject: "héllo wörld",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(built.Raw, "héllo") {
		t.Fatalf("raw non-ASCII subject must be RFC 2047 encoded, got:\n%s", built.Raw)
	}
	// internal/mail's inbound parser does not decode RFC 2047 encoded
	// words (it only ever needed to store raw header text), so verify the
	// encoding directly with the standard decoder instead.
	parsed, err := mail.ParseMessage(built.Raw)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := (&mime.WordDecoder{}).DecodeHeader(parsed.Subject)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != "héllo wörld" {
		t.Fatalf("subject did not round-trip: %q", decoded)
	}
}

func TestBuildToCcReplyToHeaders(t *testing.T) {
	built, err := Build(Request{
		From: "a@example.com", To: []string{"b@example.com", "c@example.com"},
		Cc: []string{"d@example.com"}, ReplyTo: "support@example.com", Text: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(built.Raw, "To: <b@example.com>, <c@example.com>\r\n") {
		t.Fatalf("missing/incorrect To header:\n%s", built.Raw)
	}
	if !strings.Contains(built.Raw, "Cc: <d@example.com>\r\n") {
		t.Fatalf("missing/incorrect Cc header:\n%s", built.Raw)
	}
	if !strings.Contains(built.Raw, "Reply-To: <support@example.com>\r\n") {
		t.Fatalf("missing/incorrect Reply-To header:\n%s", built.Raw)
	}
	if len(built.Envelope) != 3 {
		t.Fatalf("expected 3 envelope recipients (to+cc), got %v", built.Envelope)
	}
}
