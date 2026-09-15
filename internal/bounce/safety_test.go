package bounce

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/storage"
	"github.com/Ferousco-dev/mailx/internal/transfer"
)

func TestIsNullReversePath(t *testing.T) {
	if !IsNullReversePath("<>") || !IsNullReversePath("  <>  ") {
		t.Fatal("expected null reverse path to be recognized")
	}
	if IsNullReversePath("<a@example.com>") || IsNullReversePath("") {
		t.Fatal("non-null values must not be classified as null")
	}
}

func TestShouldGenerateNullOriginalSenderNeverBounces(t *testing.T) {
	ok, err := ShouldGenerate("<>", Failure{Class: FailurePermanentDelivery})
	if ok || !errors.Is(err, ErrOriginalSenderNull) {
		t.Fatalf("null original sender must never be eligible, got ok=%v err=%v", ok, err)
	}
}

func TestShouldGenerateEmptyOriginalSenderRejected(t *testing.T) {
	ok, err := ShouldGenerate("", Failure{Class: FailurePermanentDelivery})
	if ok || !errors.Is(err, ErrOriginalSenderEmpty) {
		t.Fatalf("empty original sender must be rejected, got ok=%v err=%v", ok, err)
	}
}

func TestShouldGeneratePermanentFailureEligible(t *testing.T) {
	ok, err := ShouldGenerate("<alice@example.com>", Failure{Class: FailurePermanentDelivery})
	if !ok || err != nil {
		t.Fatalf("permanent failure with real sender must be eligible: ok=%v err=%v", ok, err)
	}
}

func TestShouldGenerateRetryExhaustedEligible(t *testing.T) {
	ok, err := ShouldGenerate("<alice@example.com>", Failure{Class: FailureRetryExhausted, RetryExhausted: true})
	if !ok || err != nil {
		t.Fatalf("exhausted retry must be eligible: ok=%v err=%v", ok, err)
	}
}

func TestShouldGenerateAcceptedNeverBounces(t *testing.T) {
	// Even if a caller mistakenly builds a Failure-shaped value around an
	// accepted outcome, Class defaults to FailureUnknown (zero value) unless
	// explicitly set — Classify() itself refuses to build one at all
	// (ErrDeliverySucceeded), so this exercises the defense-in-depth path.
	ok, _ := ShouldGenerate("<alice@example.com>", Failure{Class: FailureUnknown})
	if ok {
		t.Fatal("unknown/unset failure class must never be eligible")
	}
}

func TestShouldGenerateLocalCancellationNeverBounces(t *testing.T) {
	ok, err := ShouldGenerate("<alice@example.com>", Failure{Class: FailureAborted})
	if ok || !errors.Is(err, ErrNotDSNEligible) {
		t.Fatalf("local cancellation must never generate a remote bounce: ok=%v err=%v", ok, err)
	}
}

func TestShouldGenerateInvalidRequestNeverBounces(t *testing.T) {
	ok, err := ShouldGenerate("<alice@example.com>", Failure{Class: FailureInvalidRequest})
	if ok || !errors.Is(err, ErrNotDSNEligible) {
		t.Fatalf("invalid local request must never generate a remote bounce: ok=%v err=%v", ok, err)
	}
}

func TestBounceEnvelopeUsesNullReversePath(t *testing.T) {
	env, err := BounceEnvelope("<alice@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	if env.MailFrom != "<>" {
		t.Fatalf("bounce envelope must use null reverse path, got %q", env.MailFrom)
	}
	if len(env.Recipients) != 1 || env.Recipients[0] != "<alice@example.com>" {
		t.Fatalf("bounce envelope recipient wrong: %+v", env.Recipients)
	}
}

func TestBounceEnvelopeRejectsNullOriginalSender(t *testing.T) {
	if _, err := BounceEnvelope("<>"); !errors.Is(err, ErrOriginalSenderNull) {
		t.Fatalf("expected ErrOriginalSenderNull, got %v", err)
	}
}

func TestBounceEnvelopeRejectsEmptyOriginalSender(t *testing.T) {
	if _, err := BounceEnvelope(""); !errors.Is(err, ErrOriginalSenderEmpty) {
		t.Fatalf("expected ErrOriginalSenderEmpty, got %v", err)
	}
}

func TestBounceEnvelopeRejectsMalformedSender(t *testing.T) {
	if _, err := BounceEnvelope("alice@example.com"); err == nil {
		t.Fatal("expected error for unbracketed sender")
	}
}

func TestBounceEnvelopeRejectsInjectedSender(t *testing.T) {
	if _, err := BounceEnvelope("<alice@example.com>\r\nRCPT TO:<victim@evil.test>"); err == nil {
		t.Fatal("expected error for CRLF-injected sender")
	}
}

// TestBounceEnvelopeAcceptedByRealSMTPServer proves — through the actual
// MailX inbound SMTP stack, not a mock — that a null reverse path is valid
// SMTP and that MailX's own transfer/client layers handle "MAIL FROM:<>"
// correctly end to end. This is the smallest possible real-stack check that
// the bounce envelope this package constructs is actually deliverable.
func TestBounceEnvelopeAcceptedByRealSMTPServer(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var receivedMailFrom string
	server, err := smtp.NewServer(smtp.DefaultConfig(), func(s smtp.Session, m mail.Message) error {
		receivedMailFrom = s.Envelope.MailFrom
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

	env, err := BounceEnvelope("<alice@example.com>")
	if err != nil {
		t.Fatal(err)
	}

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

	dsn, err := NewDSN(
		Failure{Class: FailurePermanentDelivery, SMTPCode: 550, RemoteMessage: "user unknown", Recipient: "<bob@example.com>"},
		[]string{"<bob@example.com>"}, "mailx-a.local", "d1", time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Generate(dsn, sampleOpts())
	if err != nil {
		t.Fatal(err)
	}

	res, err := svc.Transfer(context.Background(), transfer.Request{
		Destination: ln.Addr().String(),
		Envelope:    env,
		Raw:         raw,
	})
	if err != nil || !res.Accepted {
		t.Fatalf("bounce delivery failed: %v %+v", err, res)
	}
	if receivedMailFrom != "<>" {
		t.Fatalf("receiver did not see null reverse path: %q", receivedMailFrom)
	}
}

// TestBounceLoopPreventionNeverGeneratesSecondDSN is the explicit loop-
// prevention regression: a message that is itself already a bounce (null
// reverse path) must never cause MailX to generate a DSN, no matter how it
// failed to deliver.
func TestBounceLoopPreventionNeverGeneratesSecondDSN(t *testing.T) {
	for _, class := range []FailureClass{
		FailurePermanentDelivery, FailureRetryExhausted, FailureDNS, FailureNullMX,
	} {
		ok, err := ShouldGenerate("<>", Failure{Class: class})
		if ok || !errors.Is(err, ErrOriginalSenderNull) {
			t.Fatalf("class %v: bounce-of-a-bounce must be refused, got ok=%v err=%v", class, ok, err)
		}
	}
}
