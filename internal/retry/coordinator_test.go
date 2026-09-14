package retry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
)

type fakeDeliverer struct {
	result delivery.Result
	err    error
	calls  int
	ctx    context.Context
	req    delivery.Request
}

func (f *fakeDeliverer) Deliver(ctx context.Context, req delivery.Request) (delivery.Result, error) {
	f.calls++
	f.ctx = ctx
	f.req = req
	return f.result, f.err
}

func TestCoordinatorSuccessfulFirstOperation(t *testing.T) {
	deliverer := &fakeDeliverer{result: delivery.Result{
		DeliveryID: "delivery-1",
		Kind:       delivery.KindAccepted,
		Accepted:   true,
	}}
	coordinator := newTestCoordinator(t, deliverer, DefaultAttemptLimit())
	state := &State{}
	req := delivery.Request{Domain: "example.test", Raw: "message"}

	outcome, err := coordinator.Attempt(context.Background(), state, req, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if deliverer.calls != 1 || deliverer.ctx == nil || deliverer.req.Domain != req.Domain {
		t.Fatalf("deliverer calls/request = %d, %+v", deliverer.calls, deliverer.req)
	}
	if outcome.Status != StatusSucceeded || outcome.Schedule != nil || outcome.DeliveryError != nil {
		t.Fatalf("outcome = %+v", outcome)
	}
	latest, ok := state.Latest()
	if !ok || latest.Number != 1 || latest.Result.DeliveryID != "delivery-1" || latest.Decision != TerminalSuccess {
		t.Fatalf("latest = %+v, %v", latest, ok)
	}
}

func TestCoordinatorTemporaryFirstOperationReturnsSchedule(t *testing.T) {
	deliveryErr := errors.New("451 temporary failure")
	deliverer := &fakeDeliverer{
		result: delivery.Result{DeliveryID: "delivery-1", Kind: delivery.KindTransferTemporary},
		err:    deliveryErr,
	}
	coordinator := newTestCoordinator(t, deliverer, DefaultAttemptLimit())
	state := &State{}

	outcome, err := coordinator.Attempt(context.Background(), state, delivery.Request{}, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if deliverer.calls != 1 || state.Count() != 1 || outcome.Status != StatusRetryable {
		t.Fatalf("calls=%d count=%d outcome=%+v", deliverer.calls, state.Count(), outcome)
	}
	if !errors.Is(outcome.DeliveryError, deliveryErr) {
		t.Fatalf("DeliveryError = %v", outcome.DeliveryError)
	}
	if outcome.Schedule == nil || outcome.Schedule.Attempt != 1 || outcome.Schedule.Delay != 30*time.Minute {
		t.Fatalf("Schedule = %+v", outcome.Schedule)
	}
	wantNext := fixedNow().Add(30 * time.Minute)
	if !outcome.Schedule.NextRetryAt.Equal(wantNext) {
		t.Fatalf("NextRetryAt = %s, want %s", outcome.Schedule.NextRetryAt, wantNext)
	}
}

func TestCoordinatorTerminalFirstOperations(t *testing.T) {
	tests := []struct {
		name   string
		result delivery.Result
		err    error
		want   LifecycleStatus
	}{
		{
			name:   "permanent failure",
			result: delivery.Result{Kind: delivery.KindTransferPermanent},
			err:    errors.New("550 rejected"),
			want:   StatusFailed,
		},
		{
			name:   "Null MX",
			result: delivery.Result{Kind: delivery.KindDNSNullMX},
			err:    errors.New("Null MX"),
			want:   StatusFailed,
		},
		{
			name: "accepted with QUIT failure",
			result: delivery.Result{
				Kind:      delivery.KindAccepted,
				Accepted:  true,
				QuitError: "connection reset",
			},
			err:  errors.New("cleanup failed after acceptance"),
			want: StatusSucceeded,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deliverer := &fakeDeliverer{result: test.result, err: test.err}
			coordinator := newTestCoordinator(t, deliverer, DefaultAttemptLimit())
			state := &State{}
			outcome, err := coordinator.Attempt(context.Background(), state, delivery.Request{}, fixedNow())
			if err != nil {
				t.Fatal(err)
			}
			if deliverer.calls != 1 || outcome.Status != test.want || outcome.Schedule != nil {
				t.Fatalf("calls=%d outcome=%+v", deliverer.calls, outcome)
			}
		})
	}
}

func TestCoordinatorContinuesExistingRetryableState(t *testing.T) {
	state := temporaryState(t, 1)
	result := delivery.Result{
		DeliveryID: "delivery-2",
		Kind:       delivery.KindDNSTemporary,
		Attempts: []delivery.Attempt{
			{Destination: "mx1.example.test:25"},
			{Destination: "mx2.example.test:25"},
		},
	}
	deliverer := &fakeDeliverer{result: result, err: errors.New("temporary DNS failure")}
	coordinator := newTestCoordinator(t, deliverer, DefaultAttemptLimit())

	outcome, err := coordinator.Attempt(context.Background(), state, delivery.Request{}, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if deliverer.calls != 1 || state.Count() != 2 || outcome.Status != StatusRetryable {
		t.Fatalf("calls=%d count=%d outcome=%+v", deliverer.calls, state.Count(), outcome)
	}
	if outcome.Schedule == nil || outcome.Schedule.Attempt != 2 || outcome.Schedule.Delay != time.Hour {
		t.Fatalf("Schedule = %+v", outcome.Schedule)
	}
	history := state.History()
	if history[1].Result.DeliveryID != "delivery-2" || len(history[1].Result.Attempts) != 2 {
		t.Fatalf("history = %+v", history)
	}
}

func TestCoordinatorExistingRetryThenTerminalOutcome(t *testing.T) {
	tests := []struct {
		name   string
		result delivery.Result
		err    error
		want   LifecycleStatus
	}{
		{
			name:   "success",
			result: delivery.Result{Kind: delivery.KindAccepted, Accepted: true},
			want:   StatusSucceeded,
		},
		{
			name:   "permanent failure",
			result: delivery.Result{Kind: delivery.KindTransferPermanent},
			err:    errors.New("550 rejected"),
			want:   StatusFailed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := temporaryState(t, 1)
			deliverer := &fakeDeliverer{result: test.result, err: test.err}
			coordinator := newTestCoordinator(t, deliverer, DefaultAttemptLimit())
			outcome, err := coordinator.Attempt(context.Background(), state, delivery.Request{}, fixedNow())
			if err != nil {
				t.Fatal(err)
			}
			if deliverer.calls != 1 || state.Count() != 2 || outcome.Status != test.want || outcome.Schedule != nil {
				t.Fatalf("calls=%d count=%d outcome=%+v", deliverer.calls, state.Count(), outcome)
			}
		})
	}
}

func TestCoordinatorFinalTemporaryOperationIsExhausted(t *testing.T) {
	limit := DefaultAttemptLimit()
	state := temporaryState(t, limit.MaxAttempts-1)
	deliverer := &fakeDeliverer{
		result: delivery.Result{DeliveryID: "delivery-5", Kind: delivery.KindTransferTemporary},
		err:    errors.New("451 temporary"),
	}
	coordinator := newTestCoordinator(t, deliverer, limit)

	outcome, err := coordinator.Attempt(context.Background(), state, delivery.Request{}, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if deliverer.calls != 1 || state.Count() != limit.MaxAttempts || outcome.Status != StatusExhausted || outcome.Schedule != nil {
		t.Fatalf("calls=%d count=%d outcome=%+v", deliverer.calls, state.Count(), outcome)
	}
	latest, _ := state.Latest()
	if latest.Number != limit.MaxAttempts || latest.Decision != Retry {
		t.Fatalf("latest = %+v", latest)
	}
}

func TestCoordinatorRejectsPreexistingTerminalStatesWithoutDelivery(t *testing.T) {
	tests := []struct {
		name    string
		state   *State
		want    LifecycleStatus
		wantErr error
	}{
		{
			name:    "succeeded",
			state:   stateWithResult(t, delivery.Result{Kind: delivery.KindAccepted, Accepted: true}, nil),
			want:    StatusSucceeded,
			wantErr: ErrNotRetryable,
		},
		{
			name:    "failed",
			state:   stateWithResult(t, delivery.Result{Kind: delivery.KindTransferPermanent}, errors.New("550 rejected")),
			want:    StatusFailed,
			wantErr: ErrNotRetryable,
		},
		{
			name:    "exhausted",
			state:   temporaryState(t, DefaultAttemptLimit().MaxAttempts),
			want:    StatusExhausted,
			wantErr: ErrRetryExhausted,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deliverer := &fakeDeliverer{}
			coordinator := newTestCoordinator(t, deliverer, DefaultAttemptLimit())
			outcome, err := coordinator.Attempt(context.Background(), test.state, delivery.Request{}, fixedNow())
			if !errors.Is(err, test.wantErr) || outcome.Status != test.want {
				t.Fatalf("outcome=%+v err=%v", outcome, err)
			}
			if deliverer.calls != 0 {
				t.Fatalf("Deliver called %d times", deliverer.calls)
			}
		})
	}
}

func TestCoordinatorOneCallPerInvocation(t *testing.T) {
	deliverer := &fakeDeliverer{
		result: delivery.Result{Kind: delivery.KindTransferTemporary},
		err:    errors.New("temporary"),
	}
	coordinator := newTestCoordinator(t, deliverer, DefaultAttemptLimit())
	state := &State{}

	if _, err := coordinator.Attempt(context.Background(), state, delivery.Request{}, fixedNow()); err != nil {
		t.Fatal(err)
	}
	if deliverer.calls != 1 {
		t.Fatalf("one coordinator invocation called Deliver %d times", deliverer.calls)
	}
}

func TestCoordinatorContextCancellation(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		deliverer := &fakeDeliverer{}
		coordinator := newTestCoordinator(t, deliverer, DefaultAttemptLimit())
		state := &State{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := coordinator.Attempt(ctx, state, delivery.Request{}, fixedNow())
		if !errors.Is(err, context.Canceled) || deliverer.calls != 0 || state.Count() != 0 {
			t.Fatalf("err=%v calls=%d count=%d", err, deliverer.calls, state.Count())
		}
	})

	t.Run("returned by delivery", func(t *testing.T) {
		deliverer := &fakeDeliverer{
			result: delivery.Result{Kind: delivery.KindContext},
			err:    context.Canceled,
		}
		coordinator := newTestCoordinator(t, deliverer, DefaultAttemptLimit())
		state := &State{}

		outcome, err := coordinator.Attempt(context.Background(), state, delivery.Request{}, fixedNow())
		if err != nil {
			t.Fatal(err)
		}
		if deliverer.calls != 1 || state.Count() != 1 || outcome.Status != StatusFailed || outcome.Schedule != nil {
			t.Fatalf("calls=%d count=%d outcome=%+v", deliverer.calls, state.Count(), outcome)
		}
		if !errors.Is(outcome.DeliveryError, context.Canceled) {
			t.Fatalf("DeliveryError = %v", outcome.DeliveryError)
		}
	})
}

func TestCoordinatorValidatesBeforeDelivery(t *testing.T) {
	deliverer := &fakeDeliverer{result: delivery.Result{Kind: delivery.KindAccepted, Accepted: true}}
	tests := []struct {
		name    string
		deliver Deliverer
		backoff BackoffPolicy
		limit   AttemptLimit
		wantErr error
	}{
		{
			name:    "nil deliverer",
			backoff: DefaultBackoffPolicy(),
			limit:   DefaultAttemptLimit(),
			wantErr: ErrNilDeliverer,
		},
		{
			name:    "invalid backoff",
			deliver: deliverer,
			backoff: BackoffPolicy{},
			limit:   DefaultAttemptLimit(),
			wantErr: ErrInvalidBackoff,
		},
		{
			name:    "invalid limit",
			deliver: deliverer,
			backoff: DefaultBackoffPolicy(),
			limit:   AttemptLimit{},
			wantErr: ErrInvalidMaxAttempts,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, err := NewCoordinator(test.deliver, test.backoff, test.limit)
			if coordinator != nil || !errors.Is(err, test.wantErr) {
				t.Fatalf("NewCoordinator() = %v, %v", coordinator, err)
			}
		})
	}
	if deliverer.calls != 0 {
		t.Fatalf("invalid configuration caused %d delivery calls", deliverer.calls)
	}
}

func TestCoordinatorRejectsNilStateWithoutDelivery(t *testing.T) {
	deliverer := &fakeDeliverer{}
	coordinator := newTestCoordinator(t, deliverer, DefaultAttemptLimit())
	_, err := coordinator.Attempt(context.Background(), nil, delivery.Request{}, fixedNow())
	if !errors.Is(err, ErrNilState) || deliverer.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, deliverer.calls)
	}
}

func newTestCoordinator(t *testing.T, deliverer Deliverer, limit AttemptLimit) *Coordinator {
	t.Helper()
	coordinator, err := NewCoordinator(deliverer, DefaultBackoffPolicy(), limit)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func stateWithResult(t *testing.T, result delivery.Result, err error) *State {
	t.Helper()
	state := &State{}
	if recordErr := state.Record(result, err); recordErr != nil {
		t.Fatal(recordErr)
	}
	return state
}

func fixedNow() time.Time {
	return time.Date(2026, time.September, 14, 10, 0, 0, 0, time.UTC)
}
