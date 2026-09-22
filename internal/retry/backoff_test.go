package retry

import (
	"errors"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
)

func TestBackoffPolicyExponentialDelaysAndCap(t *testing.T) {
	policy := BackoffPolicy{Base: time.Minute, Max: 8 * time.Minute}
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: time.Minute},
		{attempt: 2, want: 2 * time.Minute},
		{attempt: 3, want: 4 * time.Minute},
		{attempt: 4, want: 8 * time.Minute},
		{attempt: 5, want: 8 * time.Minute},
		{attempt: 1000, want: 8 * time.Minute},
		{attempt: int(^uint(0) >> 1), want: 8 * time.Minute},
	}

	for _, test := range tests {
		got, err := policy.Delay(test.attempt)
		if err != nil {
			t.Fatalf("Delay(%d) error: %v", test.attempt, err)
		}
		if got != test.want {
			t.Fatalf("Delay(%d) = %s, want %s", test.attempt, got, test.want)
		}
	}
}

// TestDefaultBackoffPolicy proves the front-loaded schedule (fast first
// retries, slow later) applied to EVERY send — no separate "urgent" tier,
// no opt-in field. Delay(1) is fast enough that a momentary transient
// failure (e.g. a DNS timeout) is retried almost immediately, which is
// what makes time-critical mail (OTPs, resets) survive a blip without the
// caller doing anything differently.
func TestDefaultBackoffPolicy(t *testing.T) {
	policy := DefaultBackoffPolicy()
	wants := []time.Duration{
		5 * time.Second,
		5 * time.Minute,
		30 * time.Minute,
		2 * time.Hour,
		5 * time.Hour,
	}
	if len(policy.Schedule) != len(wants) {
		t.Fatalf("DefaultBackoffPolicy() = %+v", policy)
	}
	var total time.Duration
	for i, want := range wants {
		got, err := policy.Delay(i + 1)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("Delay(%d) = %s, want %s", i+1, got, want)
		}
		total += got
	}
	// Delay(1) must be fast enough to matter for a 5-minute OTP window —
	// the whole point of front-loading.
	if wants[0] >= 5*time.Minute {
		t.Fatalf("first retry (%s) is not fast enough to help a 5-minute OTP", wants[0])
	}
	// Beyond the schedule's length, the last entry repeats (never grows
	// unbounded, never wraps to zero/negative).
	if got, err := policy.Delay(len(wants) + 5); err != nil || got != wants[len(wants)-1] {
		t.Fatalf("Delay beyond schedule length = %s, %v; want %s repeated", got, err, wants[len(wants)-1])
	}
	t.Logf("total exhaustion across %d attempts: %s", len(wants), total)
}

func TestBackoffPolicyRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		policy  BackoffPolicy
		attempt int
		wantErr error
	}{
		{
			name:    "zero attempt",
			policy:  BackoffPolicy{Base: time.Minute, Max: time.Hour},
			attempt: 0,
			wantErr: ErrInvalidAttempt,
		},
		{
			name:    "negative attempt",
			policy:  BackoffPolicy{Base: time.Minute, Max: time.Hour},
			attempt: -1,
			wantErr: ErrInvalidAttempt,
		},
		{
			name:    "zero base",
			policy:  BackoffPolicy{Max: time.Hour},
			attempt: 1,
			wantErr: ErrInvalidBackoff,
		},
		{
			name:    "negative base",
			policy:  BackoffPolicy{Base: -time.Minute, Max: time.Hour},
			attempt: 1,
			wantErr: ErrInvalidBackoff,
		},
		{
			name:    "zero max",
			policy:  BackoffPolicy{Base: time.Minute},
			attempt: 1,
			wantErr: ErrInvalidBackoff,
		},
		{
			name:    "negative max",
			policy:  BackoffPolicy{Base: time.Minute, Max: -time.Hour},
			attempt: 1,
			wantErr: ErrInvalidBackoff,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.policy.Delay(test.attempt)
			if got != 0 {
				t.Fatalf("Delay() = %s, want 0 on error", got)
			}
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Delay() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestBackoffPolicyMaxBelowBaseCapsImmediately(t *testing.T) {
	policy := BackoffPolicy{Base: 10 * time.Minute, Max: 3 * time.Minute}
	for _, attempt := range []int{1, 2, 1000} {
		got, err := policy.Delay(attempt)
		if err != nil {
			t.Fatal(err)
		}
		if got != policy.Max {
			t.Fatalf("Delay(%d) = %s, want cap %s", attempt, got, policy.Max)
		}
	}
}

func TestBackoffPolicyAvoidsDurationOverflow(t *testing.T) {
	policy := BackoffPolicy{Base: time.Duration(1 << 61), Max: time.Duration(1<<62 + 1)}
	got, err := policy.Delay(int(^uint(0) >> 1))
	if err != nil {
		t.Fatal(err)
	}
	if got != policy.Max {
		t.Fatalf("Delay() = %s, want %s", got, policy.Max)
	}
}

func TestBackoffPolicyIsDeterministic(t *testing.T) {
	policy := BackoffPolicy{Base: 7 * time.Second, Max: time.Hour}
	want, err := policy.Delay(6)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		got, err := policy.Delay(6)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("call %d returned %s, want %s", i, got, want)
		}
	}
}

func TestBackoffAttemptNumberMatchesStateCount(t *testing.T) {
	var state State
	if err := state.Record(delivery.Result{Kind: delivery.KindDNSTemporary}, errors.New("temporary DNS failure")); err != nil {
		t.Fatal(err)
	}

	policy := BackoffPolicy{Base: time.Minute, Max: time.Hour}
	got, err := policy.Delay(state.Count())
	if err != nil {
		t.Fatal(err)
	}
	if got != time.Minute {
		t.Fatalf("delay after delivery operation %d = %s, want %s", state.Count(), got, time.Minute)
	}
}
