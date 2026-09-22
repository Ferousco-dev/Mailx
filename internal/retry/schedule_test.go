package retry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
)

func TestNextScheduleRejectsEmptyState(t *testing.T) {
	now := time.Date(2026, time.September, 14, 10, 0, 0, 0, time.UTC)
	for _, state := range []*State{nil, {}} {
		if _, err := NextSchedule(state, DefaultBackoffPolicy(), DefaultAttemptLimit(), now); !errors.Is(err, ErrNoDeliveryAttempts) {
			t.Fatalf("NextSchedule() error = %v, want ErrNoDeliveryAttempts", err)
		}
	}
}

func TestNextScheduleUsesRetryLevelAttemptNumber(t *testing.T) {
	now := time.Date(2026, time.September, 14, 10, 0, 0, 0, time.UTC)
	policy := DefaultBackoffPolicy()
	state := &State{}
	wants := []struct {
		delay time.Duration
		next  time.Time
	}{
		{delay: 5 * time.Second, next: time.Date(2026, time.September, 14, 10, 0, 5, 0, time.UTC)},
		{delay: 5 * time.Minute, next: time.Date(2026, time.September, 14, 10, 5, 0, 0, time.UTC)},
		{delay: 30 * time.Minute, next: time.Date(2026, time.September, 14, 10, 30, 0, 0, time.UTC)},
	}

	for i, want := range wants {
		result := delivery.Result{
			DeliveryID: "temporary",
			Kind:       delivery.KindTransferTemporary,
			// Multiple MX attempts still constitute one retry-level operation.
			Attempts: []delivery.Attempt{{Destination: "mx1:25"}, {Destination: "mx2:25"}},
		}
		if err := state.Record(result, errors.New("temporary failure")); err != nil {
			t.Fatal(err)
		}

		schedule, err := NextSchedule(state, policy, DefaultAttemptLimit(), now)
		if err != nil {
			t.Fatal(err)
		}
		if schedule.Attempt != i+1 || schedule.Attempt != state.Count() {
			t.Fatalf("Attempt = %d, State.Count() = %d", schedule.Attempt, state.Count())
		}
		if schedule.Delay != want.delay || !schedule.NextRetryAt.Equal(want.next) {
			t.Fatalf("schedule = %+v, want delay %s and next %s", schedule, want.delay, want.next)
		}
	}
}

func TestNextScheduleUsesCappedDelay(t *testing.T) {
	state := temporaryState(t, 5)
	policy := BackoffPolicy{Base: time.Minute, Max: 4 * time.Minute}
	now := time.Date(2026, time.September, 14, 10, 0, 0, 0, time.UTC)

	schedule, err := NextSchedule(state, policy, AttemptLimit{MaxAttempts: 6}, now)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.Attempt != 5 || schedule.Delay != 4*time.Minute || !schedule.NextRetryAt.Equal(now.Add(4*time.Minute)) {
		t.Fatalf("schedule = %+v", schedule)
	}
}

func TestNextScheduleRejectsTerminalState(t *testing.T) {
	tests := []struct {
		name   string
		result delivery.Result
		err    error
	}{
		{
			name:   "terminal success",
			result: delivery.Result{Kind: delivery.KindAccepted, Accepted: true},
		},
		{
			name: "accepted with QUIT error",
			result: delivery.Result{
				Kind:      delivery.KindAccepted,
				Accepted:  true,
				QuitError: "connection reset",
			},
		},
		{
			name:   "terminal failure",
			result: delivery.Result{Kind: delivery.KindDNSNotFound},
			err:    errors.New("domain not found"),
		},
		{
			name:   "Null MX",
			result: delivery.Result{Kind: delivery.KindDNSNullMX},
			err:    errors.New("domain does not accept mail"),
		},
		{
			name:   "permanent SMTP failure",
			result: delivery.Result{Kind: delivery.KindTransferPermanent},
			err:    errors.New("550 rejected"),
		},
		{
			name:   "caller cancellation",
			result: delivery.Result{Kind: delivery.KindContext},
			err:    context.Canceled,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := &State{}
			if err := state.Record(test.result, test.err); err != nil {
				t.Fatal(err)
			}
			if _, err := NextSchedule(state, DefaultBackoffPolicy(), DefaultAttemptLimit(), time.Now()); !errors.Is(err, ErrNotRetryable) {
				t.Fatalf("NextSchedule() error = %v, want ErrNotRetryable", err)
			}
		})
	}
}

func TestNextScheduleIsDeterministicAndNormalizesUTC(t *testing.T) {
	state := temporaryState(t, 1)
	zone := time.FixedZone("WAT", 60*60)
	now := time.Date(2026, time.September, 14, 11, 0, 0, 123, zone)
	want := time.Date(2026, time.September, 14, 10, 0, 5, 123, time.UTC)

	first, err := NextSchedule(state, DefaultBackoffPolicy(), DefaultAttemptLimit(), now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NextSchedule(state, DefaultBackoffPolicy(), DefaultAttemptLimit(), now)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("repeated schedules differ: %+v and %+v", first, second)
	}
	if first.NextRetryAt.Location() != time.UTC || !first.NextRetryAt.Equal(want) {
		t.Fatalf("NextRetryAt = %s (%s), want %s UTC", first.NextRetryAt, first.NextRetryAt.Location(), want)
	}
}

func TestNextScheduleRejectsTimeOverflow(t *testing.T) {
	state := temporaryState(t, 1)
	policy := BackoffPolicy{Base: 30 * time.Minute, Max: time.Hour}
	tests := []time.Time{
		time.Date(9999, time.December, 31, 23, 45, 0, 0, time.UTC),
		time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC),
		time.Date(0, time.December, 31, 23, 0, 0, 0, time.UTC),
	}
	for _, now := range tests {
		if _, err := NextSchedule(state, policy, DefaultAttemptLimit(), now); !errors.Is(err, ErrInvalidScheduleTime) {
			t.Fatalf("NextSchedule(%s) error = %v, want ErrInvalidScheduleTime", now, err)
		}
	}
}

func TestNextSchedulePropagatesBackoffErrors(t *testing.T) {
	state := temporaryState(t, 1)
	_, err := NextSchedule(state, BackoffPolicy{}, DefaultAttemptLimit(), time.Now())
	if !errors.Is(err, ErrInvalidBackoff) {
		t.Fatalf("NextSchedule() error = %v, want ErrInvalidBackoff", err)
	}
}

func temporaryState(t *testing.T, attempts int) *State {
	t.Helper()
	state := &State{}
	for i := 0; i < attempts; i++ {
		if err := state.Record(delivery.Result{Kind: delivery.KindDNSTemporary}, errors.New("temporary DNS failure")); err != nil {
			t.Fatal(err)
		}
	}
	return state
}
