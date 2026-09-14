package retry

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/transfer"
)

func TestAuditAcceptedOutcomeCannotRedeliver(t *testing.T) {
	tests := []struct {
		name   string
		result delivery.Result
		err    error
	}{
		{name: "normal", result: delivery.Result{Kind: delivery.KindAccepted, Accepted: true}},
		{
			name:   "QUIT error",
			result: delivery.Result{Kind: delivery.KindAccepted, Accepted: true, QuitError: "connection reset"},
			err:    errors.New("cleanup failed"),
		},
		{
			name:   "wrapped error",
			result: delivery.Result{Kind: delivery.KindAccepted, Accepted: true},
			err:    fmt.Errorf("post-acceptance failure: %w", errors.New("write failed")),
		},
		{
			name:   "cancellation observed afterward",
			result: delivery.Result{Kind: delivery.KindContext, Accepted: true},
			err:    fmt.Errorf("cleanup: %w", context.Canceled),
		},
		{
			name:   "deadline observed afterward",
			result: delivery.Result{Kind: delivery.KindTransferTemporary, Accepted: true},
			err:    fmt.Errorf("cleanup: %w", context.DeadlineExceeded),
		},
		{
			name:   "inconsistent permanent kind",
			result: delivery.Result{Kind: delivery.KindTransferPermanent, Accepted: true},
			err:    errors.New("inconsistent later error"),
		},
		{
			name:   "incomplete result",
			result: delivery.Result{Accepted: true},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limit := DefaultAttemptLimit()
			state := temporaryState(t, limit.MaxAttempts-1)
			deliverer := &fakeDeliverer{result: test.result, err: test.err}
			coordinator := newTestCoordinator(t, deliverer, limit)

			outcome, err := coordinator.Attempt(context.Background(), state, delivery.Request{}, fixedNow())
			if err != nil {
				t.Fatal(err)
			}
			if deliverer.calls != 1 || outcome.Status != StatusSucceeded || outcome.Schedule != nil {
				t.Fatalf("accepted outcome = calls %d, outcome %+v", deliverer.calls, outcome)
			}
			if state.Count() != limit.MaxAttempts {
				t.Fatalf("state count = %d, want %d", state.Count(), limit.MaxAttempts)
			}
			latest, ok := state.Latest()
			if !ok || latest.Decision != TerminalSuccess {
				t.Fatalf("latest = %+v, present %v", latest, ok)
			}

			blocked, err := coordinator.Attempt(context.Background(), state, delivery.Request{}, fixedNow())
			if !errors.Is(err, ErrNotRetryable) || blocked.Status != StatusSucceeded {
				t.Fatalf("second call = outcome %+v, error %v", blocked, err)
			}
			if deliverer.calls != 1 || state.Count() != limit.MaxAttempts {
				t.Fatalf("accepted message redelivered: calls %d, count %d", deliverer.calls, state.Count())
			}
		})
	}
}

func TestAuditCoordinatorRejectsExpiredDeadlineBeforeDelivery(t *testing.T) {
	deliverer := &fakeDeliverer{}
	coordinator := newTestCoordinator(t, deliverer, DefaultAttemptLimit())
	state := &State{}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	outcome, err := coordinator.Attempt(ctx, state, delivery.Request{}, fixedNow())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Attempt() error = %v, want context deadline exceeded", err)
	}
	if outcome.Status != StatusEmpty || deliverer.calls != 0 || state.Count() != 0 {
		t.Fatalf("expired deadline mutated lifecycle: outcome %+v, calls %d, count %d", outcome, deliverer.calls, state.Count())
	}
}

func TestAuditStateSnapshotsDeeplyProtectNestedHistory(t *testing.T) {
	transferErr := &transfer.TransferError{Destination: "mx1.example.test:25", Temporary: true}
	state := &State{}
	err := state.Record(delivery.Result{
		Kind: delivery.KindTransferTemporary,
		Attempts: []delivery.Attempt{{
			Destination: "mx1.example.test:25",
			TransferErr: transferErr,
		}},
		MXCandidates: []dns.MX{{Host: "mx1.example.test", Preference: 10}},
	}, transferErr)
	if err != nil {
		t.Fatal(err)
	}

	history := state.History()
	history[0].Result.Attempts[0].Destination = "mutated:25"
	history[0].Result.Attempts[0].TransferErr.Destination = "mutated:25"
	history[0].Result.MXCandidates[0].Host = "mutated.example.test"

	latest, ok := state.Latest()
	if !ok {
		t.Fatal("Latest() returned no attempt")
	}
	assertNestedSnapshotUnchanged(t, latest)

	latest.Result.Attempts[0].Destination = "mutated-again:25"
	latest.Result.Attempts[0].TransferErr.Destination = "mutated-again:25"
	latest.Result.MXCandidates[0].Host = "mutated-again.example.test"
	latestAgain, _ := state.Latest()
	assertNestedSnapshotUnchanged(t, latestAgain)
}

func assertNestedSnapshotUnchanged(t *testing.T, attempt DeliveryAttempt) {
	t.Helper()
	if attempt.Result.Attempts[0].Destination != "mx1.example.test:25" ||
		attempt.Result.Attempts[0].TransferErr.Destination != "mx1.example.test:25" ||
		attempt.Result.MXCandidates[0].Host != "mx1.example.test" {
		t.Fatalf("nested state snapshot was mutated: %+v", attempt.Result)
	}
}

func TestAuditAttemptCountAboveLimitRemainsExhausted(t *testing.T) {
	state := temporaryState(t, 3)
	status, err := Evaluate(state, AttemptLimit{MaxAttempts: 2})
	if err != nil || status != StatusExhausted {
		t.Fatalf("Evaluate() = %v, %v; want StatusExhausted", status, err)
	}
	latest, _ := state.Latest()
	if latest.Number != 3 || latest.Decision != Retry {
		t.Fatalf("exhaustion rewrote history: %+v", latest)
	}
}

func TestAuditScheduleExactMaximumTimestamp(t *testing.T) {
	state := temporaryState(t, 1)
	policy := BackoffPolicy{Base: time.Nanosecond, Max: time.Nanosecond}
	now := maxScheduleTime.Add(-time.Nanosecond)

	schedule, err := NextSchedule(state, policy, DefaultAttemptLimit(), now)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.NextRetryAt.Location() != time.UTC || !schedule.NextRetryAt.Equal(maxScheduleTime) {
		t.Fatalf("NextRetryAt = %s, want %s", schedule.NextRetryAt, maxScheduleTime)
	}
}

func FuzzBackoffPolicyDelay(f *testing.F) {
	f.Add(1, int64(1), int64(1))
	f.Add(2, int64(time.Minute), int64(time.Hour))
	f.Add(int(^uint(0)>>1), int64(1), int64(^uint64(0)>>1))
	f.Add(-1, int64(time.Minute), int64(time.Hour))
	f.Add(1, int64(-1), int64(time.Hour))

	f.Fuzz(func(t *testing.T, attempt int, baseNanos, maxNanos int64) {
		policy := BackoffPolicy{Base: time.Duration(baseNanos), Max: time.Duration(maxNanos)}
		delay, err := policy.Delay(attempt)
		valid := attempt > 0 && policy.Base > 0 && policy.Max > 0
		if !valid {
			if err == nil || delay != 0 {
				t.Fatalf("invalid Delay(%d, %s, %s) = %s, %v", attempt, policy.Base, policy.Max, delay, err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if delay <= 0 || delay > policy.Max {
			t.Fatalf("Delay(%d) = %s, max %s", attempt, delay, policy.Max)
		}
		again, err := policy.Delay(attempt)
		if err != nil || again != delay {
			t.Fatalf("Delay is not deterministic: %s then %s, error %v", delay, again, err)
		}
	})
}

func FuzzAcceptedResultNeverRetries(f *testing.F) {
	f.Add(string(delivery.KindAccepted), uint8(0))
	f.Add(string(delivery.KindTransferTemporary), uint8(1))
	f.Add("future-kind", uint8(2))

	f.Fuzz(func(t *testing.T, kind string, errorCase uint8) {
		var deliveryErr error
		switch errorCase % 4 {
		case 1:
			deliveryErr = errors.New("post-acceptance error")
		case 2:
			deliveryErr = fmt.Errorf("wrapped: %w", context.Canceled)
		case 3:
			deliveryErr = fmt.Errorf("wrapped: %w", context.DeadlineExceeded)
		}
		result := delivery.Result{Kind: delivery.Kind(kind), Accepted: true}
		if decision := Decide(result, deliveryErr); decision != TerminalSuccess {
			t.Fatalf("accepted result decision = %v", decision)
		}
		state := &State{}
		if err := state.Record(result, deliveryErr); err != nil {
			t.Fatal(err)
		}
		status, err := Evaluate(state, DefaultAttemptLimit())
		if err != nil || status != StatusSucceeded {
			t.Fatalf("accepted state = %v, %v", status, err)
		}
		if _, err := NextSchedule(state, DefaultBackoffPolicy(), DefaultAttemptLimit(), fixedNow()); !errors.Is(err, ErrNotRetryable) {
			t.Fatalf("accepted NextSchedule() error = %v", err)
		}
		if err := state.Record(delivery.Result{Kind: delivery.KindDNSTemporary}, errors.New("retry")); !errors.Is(err, ErrTerminalState) {
			t.Fatalf("accepted State.Record() error = %v", err)
		}
	})
}
