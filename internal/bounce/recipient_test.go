package bounce

import (
	"testing"

	"github.com/Ferousco-dev/mailx/internal/delivery"
)

func TestRecipientStatusesSingleRecipientPermanentFailure(t *testing.T) {
	f := Failure{
		Class:         FailurePermanentDelivery,
		SMTPCode:      550,
		RemoteMessage: "user unknown",
		Recipient:     "<bob@example.com>",
	}
	got := RecipientStatuses(f, []string{"<bob@example.com>"})
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].Action != ActionFailed || got[0].SMTPCode != 550 || got[0].Diagnostic != "user unknown" {
		t.Fatalf("wrong status: %+v", got[0])
	}
}

func TestRecipientStatusesMultipleRecipientsSharedFailure(t *testing.T) {
	// No f.Recipient set (e.g. dial failure) — every recipient shares the
	// same diagnostic; none is misattributed a per-recipient cause.
	f := Failure{
		Class:         FailureDNS,
		RemoteMessage: "",
		SMTPCode:      0,
	}
	got := RecipientStatuses(f, []string{"<a@example.com>", "<b@example.com>"})
	if len(got) != 2 {
		t.Fatalf("len = %d", len(got))
	}
	for _, s := range got {
		if s.Action != ActionFailed || s.SMTPCode != 0 {
			t.Fatalf("expected uniform shared failure, got %+v", s)
		}
	}
}

func TestRecipientStatusesRejectedSiblingNotMisattributed(t *testing.T) {
	// Recipient B caused the RCPT rejection; recipient A must NOT receive
	// B's diagnostic/code — MailX never confirmed or refused A individually.
	f := Failure{
		Class:         FailurePermanentDelivery,
		SMTPCode:      550,
		RemoteMessage: "mailbox unavailable",
		Recipient:     "<b@example.com>",
	}
	got := RecipientStatuses(f, []string{"<a@example.com>", "<b@example.com>"})
	if len(got) != 2 {
		t.Fatalf("len = %d", len(got))
	}
	var a, b RecipientStatus
	for _, s := range got {
		if s.FinalRecipient == "<a@example.com>" {
			a = s
		}
		if s.FinalRecipient == "<b@example.com>" {
			b = s
		}
	}
	if b.SMTPCode != 550 || b.Diagnostic != "mailbox unavailable" {
		t.Fatalf("rejected recipient wrong: %+v", b)
	}
	if a.SMTPCode != 0 || a.Diagnostic != abortedDiagnostic {
		t.Fatalf("sibling recipient must not inherit b's diagnostic, got %+v", a)
	}
}

func TestRecipientStatusesRetryExhaustion(t *testing.T) {
	f := Failure{
		Class:          FailureRetryExhausted,
		RetryExhausted: true,
		SMTPCode:       451,
		RemoteMessage:  "greylisted",
	}
	got := RecipientStatuses(f, []string{"<a@example.com>"})
	if got[0].SMTPCode != 451 || got[0].Action != ActionFailed {
		t.Fatalf("exhaustion status wrong: %+v", got[0])
	}
}

func TestRecipientStatusesEnhancedStatusPreserved(t *testing.T) {
	es := EnhancedStatus{Class: 5, Subject: 1, Detail: 1}
	f := Failure{Class: FailurePermanentDelivery, SMTPCode: 550, EnhancedStatus: &es, Recipient: "<a@example.com>"}
	got := RecipientStatuses(f, []string{"<a@example.com>"})
	if got[0].Status == nil || got[0].Status.String() != "5.1.1" {
		t.Fatalf("enhanced status lost: %+v", got[0])
	}
}

func TestRecipientStatusesNullMX(t *testing.T) {
	f := Failure{Class: FailureNullMX}
	got := RecipientStatuses(f, []string{"<a@example.com>", "<b@example.com>"})
	for _, s := range got {
		if s.Action != ActionFailed {
			t.Fatalf("Null MX must classify every recipient failed, got %+v", s)
		}
	}
}

func TestRecipientStatusesDNSFailure(t *testing.T) {
	f := Failure{Class: FailureDNS}
	got := RecipientStatuses(f, []string{"<a@example.com>"})
	if got[0].Action != ActionFailed {
		t.Fatalf("DNS failure must be Failed, got %+v", got[0])
	}
}

func TestRecipientStatusesLocalCancellation(t *testing.T) {
	f := Failure{Class: FailureAborted}
	got := RecipientStatuses(f, []string{"<a@example.com>"})
	if got[0].Action != ActionFailed {
		t.Fatalf("aborted must still be Failed for DSN eligibility purposes: %+v", got[0])
	}
}

func TestRecipientStatusesMalformedRecipientPassthrough(t *testing.T) {
	// Even a malformed/odd recipient string must not panic and must not
	// silently vanish from the output.
	got := RecipientStatuses(Failure{}, []string{"not-a-valid-address", ""})
	if len(got) != 2 {
		t.Fatalf("expected 2 statuses (including empty string), got %d", len(got))
	}
}

func TestRecipientStatusesEmptyEnvelope(t *testing.T) {
	got := RecipientStatuses(Failure{Recipient: "<a@example.com>"}, nil)
	if len(got) != 0 {
		t.Fatalf("expected no statuses for empty recipient list, got %+v", got)
	}
}

func TestDeliveredRecipientStatuses(t *testing.T) {
	result := delivery.Result{Accepted: true, FinalCode: 250, RemoteMessage: "accepted"}
	got := DeliveredRecipientStatuses([]string{"<a@example.com>", "<b@example.com>"}, result)
	if len(got) != 2 {
		t.Fatalf("len = %d", len(got))
	}
	for _, s := range got {
		if s.Action != ActionDelivered || s.SMTPCode != 250 {
			t.Fatalf("expected delivered, got %+v", s)
		}
	}
}

// RecipientStatuses is a pure function over its inputs (no shared mutable
// state); this documents and locks that read-only contract.
func TestRecipientStatusesDoesNotMutateFailure(t *testing.T) {
	es := EnhancedStatus{Class: 5, Subject: 1, Detail: 1}
	f := Failure{SMTPCode: 550, EnhancedStatus: &es, Recipient: "<a@example.com>"}
	before := f
	_ = RecipientStatuses(f, []string{"<a@example.com>", "<b@example.com>"})
	if f != before {
		t.Fatalf("Failure was mutated: before=%+v after=%+v", before, f)
	}
}
