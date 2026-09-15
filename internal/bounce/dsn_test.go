package bounce

import (
	"errors"
	"testing"
	"time"
)

func TestFailureEligible(t *testing.T) {
	eligible := []FailureClass{FailurePermanentDelivery, FailureRetryExhausted, FailureDNS, FailureNullMX}
	for _, c := range eligible {
		if !(Failure{Class: c}).Eligible() {
			t.Errorf("%v should be eligible", c)
		}
	}
	notEligible := []FailureClass{FailureInvalidRequest, FailureAborted, FailureUnknown}
	for _, c := range notEligible {
		if (Failure{Class: c}).Eligible() {
			t.Errorf("%v must not be eligible", c)
		}
	}
}

func TestNewDSNRejectsIneligible(t *testing.T) {
	_, err := NewDSN(Failure{Class: FailureInvalidRequest}, []string{"<a@example.com>"}, "mailx.local", "id1", time.Now())
	if !errors.Is(err, ErrNotDSNEligible) {
		t.Fatalf("expected ErrNotDSNEligible, got %v", err)
	}
}

func TestNewDSNRejectsAbortedLocalCancellation(t *testing.T) {
	_, err := NewDSN(Failure{Class: FailureAborted}, []string{"<a@example.com>"}, "mailx.local", "id1", time.Now())
	if !errors.Is(err, ErrNotDSNEligible) {
		t.Fatalf("local cancellation must never produce a DSN, got %v", err)
	}
}

func TestNewDSNRejectsEmptyReportingMTA(t *testing.T) {
	_, err := NewDSN(Failure{Class: FailurePermanentDelivery}, []string{"<a@example.com>"}, "  ", "id1", time.Now())
	if err == nil {
		t.Fatal("expected error for empty reporting MTA")
	}
}

func TestNewDSNRejectsNoRecipients(t *testing.T) {
	_, err := NewDSN(Failure{Class: FailurePermanentDelivery}, nil, "mailx.local", "id1", time.Now())
	if err == nil {
		t.Fatal("expected error for no recipients")
	}
}

func TestNewDSNPermanentFailureBuildsModel(t *testing.T) {
	f := Failure{Class: FailurePermanentDelivery, SMTPCode: 550, RemoteMessage: "user unknown", Recipient: "<bob@example.com>"}
	arrival := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	dsn, err := NewDSN(f, []string{"<bob@example.com>"}, "mailx-a.local", "delivery-1", arrival)
	if err != nil {
		t.Fatal(err)
	}
	if dsn.ReportingMTA != "mailx-a.local" || dsn.OriginalEnvelopeID != "delivery-1" {
		t.Fatalf("header fields wrong: %+v", dsn)
	}
	if !dsn.ArrivalDate.Equal(arrival) {
		t.Fatalf("arrival date wrong: %v", dsn.ArrivalDate)
	}
	if len(dsn.Recipients) != 1 || dsn.Recipients[0].Action != ActionFailed {
		t.Fatalf("recipients wrong: %+v", dsn.Recipients)
	}
}

func TestNewDSNRetryExhaustionEligible(t *testing.T) {
	f := Failure{Class: FailureRetryExhausted, RetryExhausted: true, SMTPCode: 451}
	dsn, err := NewDSN(f, []string{"<a@example.com>"}, "mailx-a.local", "id2", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !dsn.ArrivalDate.IsZero() {
		t.Fatalf("expected zero arrival date when not supplied, got %v", dsn.ArrivalDate)
	}
	if !dsn.Failure.RetryExhausted {
		t.Fatalf("exhaustion flag lost: %+v", dsn.Failure)
	}
}

func TestNewDSNNullMXEligibleWithoutSMTPSession(t *testing.T) {
	f := Failure{Class: FailureNullMX}
	dsn, err := NewDSN(f, []string{"<a@example.com>"}, "mailx-a.local", "id3", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if dsn.Recipients[0].SMTPCode != 0 {
		t.Fatalf("Null MX must not fabricate an SMTP code, got %+v", dsn.Recipients[0])
	}
}

func TestNewDSNDNSFailureEligible(t *testing.T) {
	f := Failure{Class: FailureDNS}
	if _, err := NewDSN(f, []string{"<a@example.com>"}, "mailx-a.local", "id4", time.Now()); err != nil {
		t.Fatal(err)
	}
}
