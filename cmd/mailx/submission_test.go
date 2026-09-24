package main

import (
	"reflect"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/smtp"
)

func TestSubmissionEnvelopePreservesVisibleHeadersAndCapturesHiddenBcc(t *testing.T) {
	// RCPT TO paths are always bracket-form ("<addr>"), never how the
	// parsed headers represent the same address (bare, or "Name <addr>")
	// - this mismatch is exactly what a naive exact-string comparison gets
	// wrong, so the fixture deliberately uses different representations
	// for the same visible recipients.
	s := smtp.Session{Envelope: mail.Envelope{
		MailFrom:   "<envelope-sender@example.com>",
		Recipients: []string{"<to@example.com>", "<cc@example.com>", "<hidden-bcc@example.com>"},
	}}
	m := mail.Message{
		From: "header-from@example.com",
		To:   []string{"to@example.com"},
		Cc:   []string{"Cc Person <cc@example.com>"},
	}

	from, to, cc, bcc := submissionEnvelope(s, m)

	if from != "<envelope-sender@example.com>" {
		t.Fatalf("expected envelope MAIL FROM, got %q", from)
	}
	if !reflect.DeepEqual(to, []string{"to@example.com"}) {
		t.Fatalf("expected visible To to come from headers, got %v", to)
	}
	if !reflect.DeepEqual(cc, []string{"Cc Person <cc@example.com>"}) {
		t.Fatalf("expected visible Cc to come from headers, got %v", cc)
	}
	if !reflect.DeepEqual(bcc, []string{"<hidden-bcc@example.com>"}) {
		t.Fatalf("expected exactly the envelope-only recipient as Bcc (no false positives from format mismatch), got %v", bcc)
	}
}

func TestSubmissionEnvelopeFallsBackToHeadersWhenEnvelopeEmpty(t *testing.T) {
	s := smtp.Session{}
	m := mail.Message{From: "header-from@example.com", To: []string{"to@example.com"}, Bcc: []string{"explicit-bcc@example.com"}}

	from, to, cc, bcc := submissionEnvelope(s, m)

	if from != "header-from@example.com" {
		t.Fatalf("expected header From fallback, got %q", from)
	}
	if !reflect.DeepEqual(to, []string{"to@example.com"}) {
		t.Fatalf("expected header To fallback, got %v", to)
	}
	if cc != nil {
		t.Fatalf("expected no Cc, got %v", cc)
	}
	if !reflect.DeepEqual(bcc, []string{"explicit-bcc@example.com"}) {
		t.Fatalf("expected header Bcc fallback, got %v", bcc)
	}
}
