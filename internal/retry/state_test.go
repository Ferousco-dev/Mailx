package retry

import (
	"errors"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/transfer"
)

func TestZeroValueStateIsEmpty(t *testing.T) {
	var state State
	if state.Count() != 0 {
		t.Fatalf("Count() = %d, want 0", state.Count())
	}
	if _, ok := state.Latest(); ok {
		t.Fatal("Latest() reported an attempt for an empty state")
	}
	if history := state.History(); len(history) != 0 {
		t.Fatalf("History() length = %d, want 0", len(history))
	}
}

func TestStateRecordsDeliveryOperationsAndPreservesHistory(t *testing.T) {
	var state State
	firstErr := errors.New("temporary DNS failure")
	first := delivery.Result{
		DeliveryID: "delivery-1",
		Kind:       delivery.KindDNSTemporary,
		Attempts: []delivery.Attempt{
			{Destination: "mx1.example.test:25"},
			{Destination: "mx2.example.test:25"},
		},
		MXCandidates: []dns.MX{
			{Host: "mx1.example.test", Preference: 10},
			{Host: "mx2.example.test", Preference: 20},
		},
	}
	if err := state.Record(first, firstErr); err != nil {
		t.Fatal(err)
	}

	first.DeliveryID = "mutated"
	first.Attempts[0].Destination = "mutated:25"
	first.MXCandidates[0].Host = "mutated"

	second := delivery.Result{
		DeliveryID: "delivery-2",
		Kind:       delivery.KindAccepted,
		Accepted:   true,
		Attempts:   []delivery.Attempt{{Destination: "mx3.example.test:25"}},
	}
	if err := state.Record(second, nil); err != nil {
		t.Fatal(err)
	}

	if state.Count() != 2 {
		t.Fatalf("Count() = %d, want 2", state.Count())
	}
	latest, ok := state.Latest()
	if !ok || latest.Number != 2 || latest.Result.DeliveryID != "delivery-2" || latest.Decision != TerminalSuccess {
		t.Fatalf("Latest() = %+v, %v", latest, ok)
	}

	history := state.History()
	if history[0].Number != 1 || history[0].Decision != Retry || history[0].ErrorMessage != firstErr.Error() {
		t.Fatalf("first attempt = %+v", history[0])
	}
	if history[0].Result.DeliveryID != "delivery-1" {
		t.Fatalf("recorded result was mutated: %+v", history[0].Result)
	}
	if len(history[0].Result.Attempts) != 2 || history[0].Result.Attempts[0].Destination != "mx1.example.test:25" {
		t.Fatalf("MX attempt history was not preserved: %+v", history[0].Result.Attempts)
	}
	if history[0].Result.MXCandidates[0].Host != "mx1.example.test" {
		t.Fatalf("MX candidates were not preserved: %+v", history[0].Result.MXCandidates)
	}
	if len(history[1].Result.Attempts) != 1 {
		t.Fatalf("second delivery operation MX history = %+v", history[1].Result.Attempts)
	}

	history[0].Result.DeliveryID = "changed snapshot"
	again := state.History()
	if again[0].Result.DeliveryID != "delivery-1" {
		t.Fatalf("mutating History result changed state: %+v", again[0])
	}
}

func TestStateAttemptNumbersAreContiguousAndUnique(t *testing.T) {
	var state State
	for i := 0; i < 4; i++ {
		result := delivery.Result{DeliveryID: "temporary", Kind: delivery.KindTransferTemporary}
		if err := state.Record(result, errors.New("temporary failure")); err != nil {
			t.Fatal(err)
		}
	}

	for i, attempt := range state.History() {
		want := i + 1
		if attempt.Number != want {
			t.Fatalf("attempt %d has number %d, want %d", i, attempt.Number, want)
		}
	}
}

func TestStateRejectsAttemptAfterTerminalSuccess(t *testing.T) {
	var state State
	if err := state.Record(delivery.Result{Kind: delivery.KindAccepted, Accepted: true}, nil); err != nil {
		t.Fatal(err)
	}
	before := state.History()
	if err := state.Record(delivery.Result{Kind: delivery.KindDNSTemporary}, errors.New("temporary")); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("Record() error = %v, want ErrTerminalState", err)
	}
	assertHistoryUnchanged(t, state.History(), before)
}

func TestTemporaryThenAcceptedRejectsDuplicateDelivery(t *testing.T) {
	var state State
	if err := state.Record(delivery.Result{DeliveryID: "delivery-1", Kind: delivery.KindTransferTemporary}, errors.New("451 temporary")); err != nil {
		t.Fatal(err)
	}
	if err := state.Record(delivery.Result{DeliveryID: "delivery-2", Kind: delivery.KindAccepted, Accepted: true}, nil); err != nil {
		t.Fatal(err)
	}
	before := state.History()

	err := state.Record(delivery.Result{DeliveryID: "delivery-3", Kind: delivery.KindTransferTemporary}, errors.New("must not run"))
	if !errors.Is(err, ErrTerminalState) {
		t.Fatalf("Record() error = %v, want ErrTerminalState", err)
	}
	assertHistoryUnchanged(t, state.History(), before)
}

func TestStateRejectsAttemptAfterTerminalFailure(t *testing.T) {
	tests := []struct {
		name string
		kind delivery.Kind
	}{
		{name: "permanent SMTP failure", kind: delivery.KindTransferPermanent},
		{name: "Null MX", kind: delivery.KindDNSNullMX},
		{name: "invalid request", kind: delivery.KindInvalidRequest},
		{name: "unknown result", kind: delivery.Kind("unknown")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var state State
			result := delivery.Result{DeliveryID: "terminal", Kind: test.kind}
			if err := state.Record(result, errors.New("terminal failure")); err != nil {
				t.Fatal(err)
			}
			latest, _ := state.Latest()
			if latest.Decision != Decide(result, errors.New("terminal failure")) || latest.Decision != TerminalFailure {
				t.Fatalf("decision = %v", latest.Decision)
			}
			if err := state.Record(delivery.Result{Kind: delivery.KindDNSTemporary}, nil); !errors.Is(err, ErrTerminalState) {
				t.Fatalf("Record() error = %v, want ErrTerminalState", err)
			}
			if state.Count() != 1 {
				t.Fatalf("Count() = %d, want 1", state.Count())
			}
		})
	}
}

func TestAcceptedWithQUITFailureIsTerminal(t *testing.T) {
	var state State
	result := delivery.Result{
		DeliveryID: "accepted",
		Kind:       delivery.KindAccepted,
		Accepted:   true,
		QuitError:  "connection reset",
	}
	if err := state.Record(result, errors.New("later cleanup failed")); err != nil {
		t.Fatal(err)
	}
	latest, _ := state.Latest()
	if latest.Decision != TerminalSuccess {
		t.Fatalf("decision = %v, want TerminalSuccess", latest.Decision)
	}
	if err := state.Record(delivery.Result{Kind: delivery.KindDNSTemporary}, nil); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("Record() error = %v, want ErrTerminalState", err)
	}
}

func TestStateClonesTransferErrors(t *testing.T) {
	var state State
	transferErr := &transfer.TransferError{Destination: "mx.example.test:25", Temporary: true}
	result := delivery.Result{
		Kind: delivery.KindTransferTemporary,
		Attempts: []delivery.Attempt{
			{Destination: transferErr.Destination, TransferErr: transferErr},
		},
	}
	if err := state.Record(result, transferErr); err != nil {
		t.Fatal(err)
	}

	transferErr.Destination = "mutated:25"
	latest, _ := state.Latest()
	if got := latest.Result.Attempts[0].TransferErr.Destination; got != "mx.example.test:25" {
		t.Fatalf("stored transfer error destination = %q", got)
	}
}

func assertHistoryUnchanged(t *testing.T, got, want []DeliveryAttempt) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("history length changed: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Number != want[i].Number ||
			got[i].Result.DeliveryID != want[i].Result.DeliveryID ||
			got[i].Decision != want[i].Decision ||
			got[i].ErrorMessage != want[i].ErrorMessage {
			t.Fatalf("history[%d] changed: got %+v want %+v", i, got[i], want[i])
		}
	}
}
