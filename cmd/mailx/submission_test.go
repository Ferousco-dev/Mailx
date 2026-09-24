package main

import (
	"reflect"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/smtp"
)

func TestSubmissionEnvelopePrefersSMTPEnvelopeOverHeaders(t *testing.T) {
	s := smtp.Session{Envelope: mail.Envelope{
		MailFrom:   "envelope-sender@example.com",
		Recipients: []string{"to@example.com", "hidden-bcc@example.com"},
	}}
	m := mail.Message{From: "header-from@example.com", To: []string{"to@example.com"}}

	from, to := submissionEnvelope(s, m)

	if from != "envelope-sender@example.com" {
		t.Fatalf("expected envelope MAIL FROM, got %q", from)
	}
	if !reflect.DeepEqual(to, []string{"to@example.com", "hidden-bcc@example.com"}) {
		t.Fatalf("expected envelope RCPT TO list (including Bcc), got %v", to)
	}
}

func TestSubmissionEnvelopeFallsBackToHeadersWhenEnvelopeEmpty(t *testing.T) {
	s := smtp.Session{}
	m := mail.Message{From: "header-from@example.com", To: []string{"to@example.com"}}

	from, to := submissionEnvelope(s, m)

	if from != "header-from@example.com" {
		t.Fatalf("expected header From fallback, got %q", from)
	}
	if !reflect.DeepEqual(to, []string{"to@example.com"}) {
		t.Fatalf("expected header To fallback, got %v", to)
	}
}
