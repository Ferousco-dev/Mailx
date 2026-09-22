package retry

import "errors"

// ErrInvalidMaxAttempts indicates that a retry lifecycle has no positive,
// bounded total-delivery-operation limit.
var ErrInvalidMaxAttempts = errors.New("retry: max attempts must be positive")

// AttemptLimit bounds the total number of retry-level delivery operations.
// It does not count MX/SMTP attempts nested inside delivery.Result.Attempts.
type AttemptLimit struct {
	MaxAttempts int
}

// DefaultAttemptLimit returns MailX's current development policy of five total
// delivery operations. RFC 5321 does not mandate this exact number.
func DefaultAttemptLimit() AttemptLimit {
	return AttemptLimit{MaxAttempts: 5}
}

// UrgentAttemptLimit is the same operation count as DefaultAttemptLimit —
// only UrgentBackoffPolicy's delays differ. Time-critical mail (OTPs,
// password resets) needs to exhaust FAST, not retry MORE.
func UrgentAttemptLimit() AttemptLimit {
	return AttemptLimit{MaxAttempts: 5}
}

// LifecycleStatus describes the retry lifecycle without changing the latest
// delivery operation's own Decision.
type LifecycleStatus uint8

const (
	StatusEmpty LifecycleStatus = iota
	StatusRetryable
	StatusSucceeded
	StatusFailed
	StatusExhausted
)

// Evaluate reports the lifecycle status under limit. Exhaustion occurs only
// when the latest decision remains Retry and the total operation count has
// reached MaxAttempts.
func Evaluate(state *State, limit AttemptLimit) (LifecycleStatus, error) {
	if limit.MaxAttempts <= 0 {
		return StatusEmpty, ErrInvalidMaxAttempts
	}
	if state == nil {
		return StatusEmpty, nil
	}

	latest, ok := state.Latest()
	if !ok {
		return StatusEmpty, nil
	}
	switch latest.Decision {
	case TerminalSuccess:
		return StatusSucceeded, nil
	case TerminalFailure:
		return StatusFailed, nil
	case Retry:
		if state.Count() >= limit.MaxAttempts {
			return StatusExhausted, nil
		}
		return StatusRetryable, nil
	default:
		return StatusFailed, nil
	}
}
