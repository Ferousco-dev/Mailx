package retry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
)

func TestDefaultAttemptLimit(t *testing.T) {
	if got := DefaultAttemptLimit().MaxAttempts; got != 5 {
		t.Fatalf("MaxAttempts = %d, want 5", got)
	}
}

func TestEvaluateRejectsInvalidAttemptLimit(t *testing.T) {
	for _, maxAttempts := range []int{0, -1} {
		status, err := Evaluate(&State{}, AttemptLimit{MaxAttempts: maxAttempts})
		if status != StatusEmpty || !errors.Is(err, ErrInvalidMaxAttempts) {
			t.Fatalf("Evaluate(MaxAttempts=%d) = %v, %v", maxAttempts, status, err)
		}
	}
}

func TestEvaluateEmptyStateIsNotExhausted(t *testing.T) {
	for _, state := range []*State{nil, {}} {
		status, err := Evaluate(state, DefaultAttemptLimit())
		if err != nil || status != StatusEmpty {
			t.Fatalf("Evaluate(empty) = %v, %v", status, err)
		}
	}
}

func TestEvaluateTemporaryOperationsBeforeAndAtLimit(t *testing.T) {
	limit := DefaultAttemptLimit()
	state := &State{}
	for attempt := 1; attempt <= limit.MaxAttempts; attempt++ {
		result := delivery.Result{
			Kind: delivery.KindTransferTemporary,
			Attempts: []delivery.Attempt{
				{Destination: "mx1.example.test:25"},
				{Destination: "mx2.example.test:25"},
			},
		}
		if err := state.Record(result, errors.New("temporary failure")); err != nil {
			t.Fatal(err)
		}

		status, err := Evaluate(state, limit)
		if err != nil {
			t.Fatal(err)
		}
		want := StatusRetryable
		if attempt == limit.MaxAttempts {
			want = StatusExhausted
		}
		if status != want {
			t.Fatalf("attempt %d status = %v, want %v", attempt, status, want)
		}
		latest, _ := state.Latest()
		if latest.Decision != Retry {
			t.Fatalf("attempt %d decision changed to %v", attempt, latest.Decision)
		}
	}

	if state.Count() != limit.MaxAttempts {
		t.Fatalf("multiple nested MX attempts changed retry count: got %d", state.Count())
	}
}

func TestEvaluateMaxAttemptsOneExhaustsFirstTemporaryOperation(t *testing.T) {
	state := temporaryState(t, 1)
	status, err := Evaluate(state, AttemptLimit{MaxAttempts: 1})
	if err != nil || status != StatusExhausted {
		t.Fatalf("Evaluate() = %v, %v", status, err)
	}
}

func TestEvaluateAcceptedAtLimitIsSuccessNotExhausted(t *testing.T) {
	limit := DefaultAttemptLimit()
	state := temporaryState(t, limit.MaxAttempts-1)
	if err := state.Record(delivery.Result{Kind: delivery.KindAccepted, Accepted: true}, nil); err != nil {
		t.Fatal(err)
	}

	status, err := Evaluate(state, limit)
	if err != nil || status != StatusSucceeded {
		t.Fatalf("Evaluate() = %v, %v; want StatusSucceeded", status, err)
	}
	if state.Count() != limit.MaxAttempts {
		t.Fatalf("Count() = %d, want %d", state.Count(), limit.MaxAttempts)
	}
}

func TestEvaluatePermanentFailureBeforeLimitEndsLifecycle(t *testing.T) {
	state := temporaryState(t, 1)
	if err := state.Record(delivery.Result{Kind: delivery.KindTransferPermanent}, errors.New("550 rejected")); err != nil {
		t.Fatal(err)
	}

	status, err := Evaluate(state, DefaultAttemptLimit())
	if err != nil || status != StatusFailed {
		t.Fatalf("Evaluate() = %v, %v; want StatusFailed", status, err)
	}
	if state.Count() != 2 {
		t.Fatalf("Count() = %d, want 2", state.Count())
	}
}

func TestEvaluateHugeMaxAttempts(t *testing.T) {
	state := temporaryState(t, 1)
	status, err := Evaluate(state, AttemptLimit{MaxAttempts: int(^uint(0) >> 1)})
	if err != nil || status != StatusRetryable {
		t.Fatalf("Evaluate() = %v, %v", status, err)
	}
}

func TestEvaluateTerminalOutcomesWinBeforeLimit(t *testing.T) {
	tests := []struct {
		name   string
		result delivery.Result
		err    error
		want   LifecycleStatus
	}{
		{
			name:   "accepted",
			result: delivery.Result{Kind: delivery.KindAccepted, Accepted: true},
			want:   StatusSucceeded,
		},
		{
			name: "accepted with QUIT failure",
			result: delivery.Result{
				Kind:      delivery.KindAccepted,
				Accepted:  true,
				QuitError: "connection reset",
			},
			err:  errors.New("cleanup failed"),
			want: StatusSucceeded,
		},
		{
			name:   "permanent SMTP failure",
			result: delivery.Result{Kind: delivery.KindTransferPermanent},
			err:    errors.New("550 rejected"),
			want:   StatusFailed,
		},
		{
			name:   "Null MX",
			result: delivery.Result{Kind: delivery.KindDNSNullMX},
			err:    errors.New("domain accepts no mail"),
			want:   StatusFailed,
		},
		{
			name:   "caller cancellation",
			result: delivery.Result{Kind: delivery.KindContext},
			err:    context.Canceled,
			want:   StatusFailed,
		},
		{
			name:   "unknown terminal result",
			result: delivery.Result{Kind: delivery.Kind("unknown")},
			err:    errors.New("unknown"),
			want:   StatusFailed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := &State{}
			if err := state.Record(test.result, test.err); err != nil {
				t.Fatal(err)
			}
			status, err := Evaluate(state, DefaultAttemptLimit())
			if err != nil || status != test.want {
				t.Fatalf("Evaluate() = %v, %v; want %v", status, err, test.want)
			}
		})
	}
}

func TestAttemptLimitControlsScheduling(t *testing.T) {
	now := time.Date(2026, time.September, 14, 10, 0, 0, 0, time.UTC)
	backoff := DefaultBackoffPolicy()
	limit := AttemptLimit{MaxAttempts: 2}
	state := temporaryState(t, 1)

	schedule, err := NextSchedule(state, backoff, limit, now)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.Attempt != 1 {
		t.Fatalf("schedule attempt = %d, want 1", schedule.Attempt)
	}

	if err := state.Record(delivery.Result{Kind: delivery.KindDNSTemporary}, errors.New("temporary DNS failure")); err != nil {
		t.Fatal(err)
	}
	latest, _ := state.Latest()
	if latest.Decision != Retry {
		t.Fatalf("exhaustion changed delivery decision to %v", latest.Decision)
	}
	if _, err := NextSchedule(state, backoff, limit, now); !errors.Is(err, ErrRetryExhausted) {
		t.Fatalf("NextSchedule() error = %v, want ErrRetryExhausted", err)
	}
}

func TestNextScheduleRejectsInvalidAttemptLimit(t *testing.T) {
	state := temporaryState(t, 1)
	_, err := NextSchedule(state, DefaultBackoffPolicy(), AttemptLimit{}, time.Now())
	if !errors.Is(err, ErrInvalidMaxAttempts) {
		t.Fatalf("NextSchedule() error = %v, want ErrInvalidMaxAttempts", err)
	}
}
