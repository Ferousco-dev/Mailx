package mail

import "testing"

func TestEnvelopeAndMessageAreSeparate(t *testing.T) {
	envelope := Envelope{
		MailFrom:   "<sender@example.com>",
		Recipients: []string{"<visible@example.com>", "<hidden@example.com>"},
	}
	m := Message{
		From: "Alice <alice@example.com>",
		To:   []string{"visible@example.com"},
		Body: "Hello",
	}

	if len(envelope.Recipients) != 2 {
		t.Fatalf("got %d envelope recipients, want 2", len(envelope.Recipients))
	}
	if len(m.To) != 1 || m.To[0] == "hidden@example.com" {
		t.Fatal("Bcc recipient must not be copied into visible headers")
	}
}
