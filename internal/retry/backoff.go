package retry

import (
	"errors"
	"fmt"
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
}

// DefaultBackoffPolicy returns RFC-aligned initial defaults. They remain
// explicit policy values and can be replaced as MailX deployment needs evolve.
func DefaultBackoffPolicy() BackoffPolicy {
	return BackoffPolicy{
		Base: 30 * time.Minute,
		Max:  4 * time.Hour,
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
