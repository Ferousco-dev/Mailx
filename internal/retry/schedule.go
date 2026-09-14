package retry

import (
	"errors"
	"time"
)

var (
	// ErrNoDeliveryAttempts indicates that no completed delivery operation is
	// available from which to calculate a retry schedule.
	ErrNoDeliveryAttempts = errors.New("retry: no completed delivery attempts")
	// ErrNotRetryable indicates that the latest delivery operation is terminal.
	ErrNotRetryable = errors.New("retry: latest delivery attempt is terminal")
	// ErrRetryExhausted indicates that the latest failure is temporary but the
	// configured total delivery-operation limit has been reached.
	ErrRetryExhausted = errors.New("retry: maximum delivery attempts exhausted")
	// ErrInvalidScheduleTime indicates that scheduling metadata cannot represent
	// the supplied reference time plus its backoff delay.
	ErrInvalidScheduleTime = errors.New("retry: schedule time is outside supported range")
)

var (
	minScheduleTime = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
	maxScheduleTime = time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC)
)

// Schedule describes eligibility for the delivery operation following Attempt.
// Attempt is the one-based number of the most recently completed operation.
type Schedule struct {
	Attempt     int
	Delay       time.Duration
	NextRetryAt time.Time
}

// NextSchedule calculates retry eligibility metadata without waiting or
// executing delivery. The caller supplies now, which makes clock ownership
// explicit and deterministic in tests.
func NextSchedule(state *State, policy BackoffPolicy, limit AttemptLimit, now time.Time) (Schedule, error) {
	status, err := Evaluate(state, limit)
	if err != nil {
		return Schedule{}, err
	}
	switch status {
	case StatusEmpty:
		return Schedule{}, ErrNoDeliveryAttempts
	case StatusExhausted:
		return Schedule{}, ErrRetryExhausted
	case StatusSucceeded, StatusFailed:
		return Schedule{}, ErrNotRetryable
	case StatusRetryable:
		// Continue below and calculate scheduling metadata.
	default:
		return Schedule{}, ErrNotRetryable
	}

	latest, _ := state.Latest()
	delay, err := policy.Delay(latest.Number)
	if err != nil {
		return Schedule{}, err
	}

	reference := now.UTC()
	if reference.Before(minScheduleTime) || reference.After(maxScheduleTime) {
		return Schedule{}, ErrInvalidScheduleTime
	}
	if delay > maxScheduleTime.Sub(reference) {
		return Schedule{}, ErrInvalidScheduleTime
	}

	return Schedule{
		Attempt:     latest.Number,
		Delay:       delay,
		NextRetryAt: reference.Add(delay),
	}, nil
}
