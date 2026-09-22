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
//
// Two mutually exclusive shapes: an explicit Schedule (front-loaded delays,
// one per attempt, the last entry repeating for any attempt beyond its
// length — see DefaultBackoffPolicy), or the older Base/Max exponential-
// doubling shape (kept for callers that construct a BackoffPolicy
// themselves rather than using DefaultBackoffPolicy). Delay checks Schedule
// first.
type BackoffPolicy struct {
	// Schedule, if non-empty, is used instead of Base/Max: Schedule[attempt-1],
	// clamped to Schedule[len(Schedule)-1] for any attempt beyond it.
	Schedule []time.Duration
	Base     time.Duration
	Max      time.Duration
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

// DefaultBackoffPolicy is front-loaded (fast first retries, slow later),
// applied to EVERY send — there is no separate "urgent" opt-in. A transient
// failure (a momentary DNS timeout, a brief MX outage) is retried almost
// immediately, so time-critical mail (OTPs, password resets, magic links)
// recovers from a blip fast enough to still be useful, without requiring
// the caller to remember to flag anything. It still backs off to hours for
// a genuinely down destination, so this is not a retry-storm risk for bulk
// mail either — only the FIRST few retries are fast, same as every prior
// schedule considered for this project (Resend's webhook-retry schedule is
// the pattern this follows, front-loaded then slow: see the design
// discussion).
//
// Exactly 4 entries, one per DELAY between DefaultAttemptLimit's 5
// operations (attempt N's delay is used only if a 6th operation could
// still happen; a 5-operation limit means the delay after attempt 5 is
// NEVER scheduled — PR review caught an earlier 5-entry version whose last
// entry was dead code). Total exhaustion across all 4 delays: ~2h35m — a
// genuinely broken destination is abandoned faster than the old exponential
// default (~7.5h), which is an accepted, deliberate tradeoff of this
// schedule, not an oversight: see the design discussion for why "fails
// fast enough for the caller to react" outweighs "keep retrying for most
// of a day" here.
func DefaultBackoffPolicy() BackoffPolicy {
	return BackoffPolicy{
		Schedule: []time.Duration{
			5 * time.Second,
			5 * time.Minute,
			30 * time.Minute,
			2 * time.Hour,
		},
	}
}

// Delay returns the wait after completed delivery operation attempt and before
// operation attempt+1. Public attempt numbering starts at one, matching State.
// If Schedule is set it is used directly (clamped to its last entry beyond
// its length); otherwise the uncapped formula is Base * 2^(attempt-1), with
// Max capping every result, including configurations where Max is smaller
// than Base.
func (p BackoffPolicy) Delay(attempt int) (time.Duration, error) {
	if attempt <= 0 {
		return 0, fmt.Errorf("%w: got %d", ErrInvalidAttempt, attempt)
	}
	if len(p.Schedule) > 0 {
		idx := attempt - 1
		if idx >= len(p.Schedule) {
			idx = len(p.Schedule) - 1
		}
		d := p.Schedule[idx]
		if d <= 0 {
			return 0, fmt.Errorf("%w: schedule[%d] is %s", ErrInvalidBackoff, idx, d)
		}
		return d, nil
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
