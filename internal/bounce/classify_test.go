package bounce

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/retry"
)

func TestClassifyRejectsMissingAndNonFinalState(t *testing.T) {
	if _, err := Classify(nil, retry.StatusEmpty); !errors.Is(err, ErrNilState) {
		t.Fatalf("nil state error = %v", err)
	}
	if _, err := Classify(&retry.State{}, retry.StatusEmpty); !errors.Is(err, ErrNoDeliveryAttempts) {
		t.Fatalf("empty state error = %v", err)
	}
	if _, err := Classify(&retry.State{}, retry.StatusRetryable); !errors.Is(err, ErrInvalidLifecycle) {
		t.Fatalf("empty retryable state error = %v", err)
	}

	state := stateWith(t, delivery.Result{Kind: delivery.KindTransferTemporary}, errors.New("451 temporary"))
	if _, err := Classify(state, retry.StatusRetryable); !errors.Is(err, ErrNotFinal) {
		t.Fatalf("retryable state error = %v", err)
	}
}

func TestClassifyRefusesSuccessfulDelivery(t *testing.T) {
	tests := []struct {
		name   string
		result delivery.Result
		err    error
	}{
		{
			name:   "accepted",
			result: delivery.Result{Kind: delivery.KindAccepted, Accepted: true},
		},
		{
			name: "accepted with QUIT error",
			result: delivery.Result{
				Kind:      delivery.KindAccepted,
				Accepted:  true,
				QuitError: "connection reset during QUIT",
			},
			err: errors.New("post-acceptance cleanup error"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := stateWith(t, test.result, test.err)
			if _, err := Classify(state, retry.StatusSucceeded); !errors.Is(err, ErrDeliverySucceeded) {
				t.Fatalf("Classify() error = %v", err)
			}
		})
	}
}

func TestClassifyPermanentFailures(t *testing.T) {
	tests := []struct {
		name string
		kind delivery.Kind
		want FailureClass
	}{
		{name: "permanent SMTP", kind: delivery.KindTransferPermanent, want: FailurePermanentDelivery},
		{name: "Null MX", kind: delivery.KindDNSNullMX, want: FailureNullMX},
		{name: "DNS not found", kind: delivery.KindDNSNotFound, want: FailureDNS},
		{name: "terminal DNS failure", kind: delivery.KindDNSFailure, want: FailureDNS},
		{name: "invalid request", kind: delivery.KindInvalidRequest, want: FailureInvalidRequest},
		{name: "unknown", kind: delivery.Kind("future_kind"), want: FailureUnknown},
		{name: "zero kind", kind: "", want: FailureUnknown},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := stateWith(t, delivery.Result{Kind: test.kind, FailureMessage: "final failure"}, errors.New("delivery failed"))
			failure, err := Classify(state, retry.StatusFailed)
			if err != nil {
				t.Fatal(err)
			}
			if failure.Class != test.want || failure.DeliveryKind != test.kind || failure.AttemptCount != 1 {
				t.Fatalf("failure = %+v", failure)
			}
			if failure.RetryExhausted || failure.Message != "final failure" {
				t.Fatalf("failure diagnostics = %+v", failure)
			}
		})
	}
}

func TestClassifyPreservesSMTPDiagnostics(t *testing.T) {
	state := stateWith(t, delivery.Result{
		Kind:           delivery.KindTransferPermanent,
		FinalCode:      550,
		EnhancedStatus: "5.1.1",
		FailureMessage: "transfer rejected",
		RemoteMessage:  "user unknown",
		FailureStage:   "rcpt_to",
		Recipient:      "<missing@example.com>",
	}, errors.New("wrapped delivery error"))

	failure, err := Classify(state, retry.StatusFailed)
	if err != nil {
		t.Fatal(err)
	}
	if failure.SMTPCode != 550 || failure.EnhancedStatusRaw != "5.1.1" ||
		failure.EnhancedStatus == nil || failure.EnhancedStatus.String() != "5.1.1" ||
		failure.Message != "transfer rejected" || failure.RemoteMessage != "user unknown" ||
		failure.FailureStage != "rcpt_to" || failure.Recipient != "<missing@example.com>" {
		t.Fatalf("failure diagnostics = %+v", failure)
	}
}

func TestClassifyRetryExhaustedPreservesTemporaryFailure(t *testing.T) {
	tests := []struct {
		name   string
		result delivery.Result
	}{
		{
			name: "temporary SMTP",
			result: delivery.Result{
				Kind:           delivery.KindTransferTemporary,
				FinalCode:      451,
				EnhancedStatus: "4.3.0",
				FailureMessage: "temporary internal failure",
				RemoteMessage:  "try again later",
				FailureStage:   "data_response",
			},
		},
		{
			name: "temporary DNS",
			result: delivery.Result{
				Kind:           delivery.KindDNSTemporary,
				FailureMessage: "DNS server failure",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := stateWith(t, delivery.Result{Kind: delivery.KindTransferTemporary}, errors.New("temporary #1"))
			if err := state.Record(test.result, errors.New("final temporary error")); err != nil {
				t.Fatal(err)
			}

			failure, err := Classify(state, retry.StatusExhausted)
			if err != nil {
				t.Fatal(err)
			}
			if failure.Class != FailureRetryExhausted || !failure.RetryExhausted || failure.AttemptCount != 2 {
				t.Fatalf("failure = %+v", failure)
			}
			if failure.DeliveryKind != test.result.Kind || failure.SMTPCode != test.result.FinalCode ||
				failure.EnhancedStatusRaw != test.result.EnhancedStatus || failure.Message != test.result.FailureMessage {
				t.Fatalf("underlying temporary result was not preserved: %+v", failure)
			}
			if test.result.EnhancedStatus == "" && failure.EnhancedStatus != nil {
				t.Fatalf("missing enhanced status became %+v", failure.EnhancedStatus)
			}
		})
	}
}

func TestClassifyCancellationIsLocalAbort(t *testing.T) {
	state := stateWith(t, delivery.Result{
		Kind:           delivery.KindContext,
		FailureMessage: context.Canceled.Error(),
	}, context.Canceled)

	failure, err := Classify(state, retry.StatusFailed)
	if err != nil {
		t.Fatal(err)
	}
	if failure.Class != FailureAborted || failure.DeliveryKind != delivery.KindContext {
		t.Fatalf("cancellation classified as recipient failure: %+v", failure)
	}
}

func TestClassifyRejectsInconsistentLifecycle(t *testing.T) {
	retryable := stateWith(t, delivery.Result{Kind: delivery.KindTransferTemporary}, errors.New("temporary"))
	succeeded := stateWith(t, delivery.Result{Kind: delivery.KindAccepted, Accepted: true}, nil)
	failed := stateWith(t, delivery.Result{Kind: delivery.KindTransferPermanent}, errors.New("permanent"))

	tests := []struct {
		name   string
		state  *retry.State
		status retry.LifecycleStatus
	}{
		{name: "nonempty marked empty", state: failed, status: retry.StatusEmpty},
		{name: "retry marked succeeded", state: retryable, status: retry.StatusSucceeded},
		{name: "success marked failed", state: succeeded, status: retry.StatusFailed},
		{name: "failure marked exhausted", state: failed, status: retry.StatusExhausted},
		{name: "success marked retryable", state: succeeded, status: retry.StatusRetryable},
		{name: "unknown status", state: failed, status: retry.LifecycleStatus(255)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Classify(test.state, test.status); !errors.Is(err, ErrInvalidLifecycle) {
				t.Fatalf("Classify() error = %v", err)
			}
		})
	}
}

func TestClassifyIsReadOnlyAndCountsDeliveryOperations(t *testing.T) {
	state := stateWith(t, delivery.Result{
		DeliveryID: "delivery-1",
		Kind:       delivery.KindTransferTemporary,
		Attempts: []delivery.Attempt{
			{Destination: "mx1.example.com:25"},
			{Destination: "mx2.example.com:25"},
		},
		MXCandidates: []dns.MX{
			{Host: "mx1.example.com", Preference: 10},
			{Host: "mx2.example.com", Preference: 20},
		},
	}, errors.New("temporary failure"))
	before := state.History()

	failure, err := Classify(state, retry.StatusExhausted)
	if err != nil {
		t.Fatal(err)
	}
	if failure.AttemptCount != 1 {
		t.Fatalf("AttemptCount = %d, want one delivery operation", failure.AttemptCount)
	}
	if after := state.History(); !reflect.DeepEqual(after, before) {
		t.Fatalf("Classify mutated state:\nbefore: %+v\nafter:  %+v", before, after)
	}
}

func TestClassifyFinalAcceptanceAfterTemporaryFailuresProducesNoFailure(t *testing.T) {
	state := stateWith(t, delivery.Result{Kind: delivery.KindDNSTemporary}, errors.New("temporary DNS"))
	if err := state.Record(delivery.Result{
		Kind:      delivery.KindAccepted,
		Accepted:  true,
		QuitError: "QUIT failed after acceptance",
	}, errors.New("later cleanup error")); err != nil {
		t.Fatal(err)
	}

	if _, err := Classify(state, retry.StatusSucceeded); !errors.Is(err, ErrDeliverySucceeded) {
		t.Fatalf("Classify() error = %v", err)
	}
	if state.Count() != 2 {
		t.Fatalf("state count changed: %d", state.Count())
	}
}

func stateWith(t *testing.T, result delivery.Result, err error) *retry.State {
	t.Helper()
	state := &retry.State{}
	if recordErr := state.Record(result, err); recordErr != nil {
		t.Fatal(recordErr)
	}
	return state
}
