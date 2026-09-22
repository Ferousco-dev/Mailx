package retry

import (
	"errors"
	"fmt"
	"hash/fnv"
	"time"
)

var (
	// ErrInvalidAttempt indicates that backoff was requested without a valid
	// one-based retry-level delivery attempt number.
	ErrInvalidAttempt = errors.New("retry: attempt number must be positive")
	// ErrInvalidBackoff indicates that a backoff duration is zero or negative.
	ErrInvalidBackoff = errors.New("retry: backoff durations must be positive")
)

// BackoffPolicy calculates deterministic delays between delivery operations.
type BackoffPolicy struct {
	Base time.Duration
	Max  time.Duration
	// JitterPercent spreads retry times by up to +/- this percent (0-50) so many
	// messages that failed at the same moment (a remote outage) do not all retry at
	// the same moment again (a retry storm). Zero disables jitter. Jitter never
	// pushes a delay above Max or below one second.
	JitterPercent int
}

// Jitter returns delay spread by the policy's JitterPercent. It is deterministic in
// (delay, seed) so tests are exact; callers pass a seed that differs between
// messages (the retry scheduling instant does), which is what breaks
// synchronization. The result is always within [delay*(1-p), delay*(1+p)], at most
// Max and at least one second.
func (p BackoffPolicy) Jitter(delay time.Duration, seed int64) time.Duration {
	if p.JitterPercent <= 0 || delay <= 0 {
		return delay
	}
	pct := p.JitterPercent
	if pct > 50 {
		pct = 50
	}
	h := fnv.New64a()
	var b [8]byte
	for i := range b {
		b[i] = byte(uint64(seed) >> (8 * i))
	}
	_, _ = h.Write(b[:])
	// u in [-1000, 1000] -> a fraction of the jitter span.
	u := int64(h.Sum64()%2001) - 1000
	out := delay + time.Duration(int64(delay)/100*int64(pct)*u/1000)
	if p.Max > 0 && out > p.Max {
		out = p.Max
	}
	if out < time.Second {
		out = time.Second
	}
	return out
}

// DefaultBackoffPolicy returns RFC-aligned initial defaults. They remain
// explicit policy values and can be replaced as MailX deployment needs evolve.
func DefaultBackoffPolicy() BackoffPolicy {
	return BackoffPolicy{
		Base: 30 * time.Minute,
		Max:  4 * time.Hour,
	}
}

// UrgentBackoffPolicy is for time-critical mail (OTPs, password resets,
// magic links) whose value expires in minutes: DefaultBackoffPolicy's first
// retry alone (30 minutes) is already useless for a 5-minute OTP. With
// UrgentAttemptLimit's 5 operations this exhausts in 10+20+40+60+60 = 190s
// (~3.2 minutes) instead of ~7.5 hours — a genuine transient blip (like a
// momentary DNS timeout) still gets several fast retries, but MailX gives up
// and lets the caller react (fall back to SMS, show a "resend" button)
// while the value could still plausibly be used, rather than silently
// retrying for hours after the value is dead. This is a fixed, deliberately
// short schedule — it is not a general per-tenant SLA/priority system.
func UrgentBackoffPolicy() BackoffPolicy {
	return BackoffPolicy{
		Base: 10 * time.Second,
		Max:  60 * time.Second,
	}
}

// Delay returns the wait after completed delivery operation attempt and before
// operation attempt+1. Public attempt numbering starts at one, matching State.
// The uncapped formula is Base * 2^(attempt-1); Max caps every result, including
// configurations where Max is smaller than Base.
func (p BackoffPolicy) Delay(attempt int) (time.Duration, error) {
	if attempt <= 0 {
		return 0, fmt.Errorf("%w: got %d", ErrInvalidAttempt, attempt)
	}
	if p.Base <= 0 {
		return 0, fmt.Errorf("%w: base is %s", ErrInvalidBackoff, p.Base)
	}
	if p.Max <= 0 {
		return 0, fmt.Errorf("%w: max is %s", ErrInvalidBackoff, p.Max)
	}
	if p.Base >= p.Max {
		return p.Max, nil
	}

	delay := p.Base
	for current := 1; current < attempt; current++ {
		// Check before multiplication so time.Duration cannot overflow.
		if delay > p.Max/2 {
			return p.Max, nil
		}
		delay *= 2
	}
	return delay, nil
}
