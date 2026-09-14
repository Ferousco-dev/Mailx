package retry

import (
	"context"
	"errors"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
)

var (
	// ErrNilDeliverer indicates that a Coordinator has no delivery dependency.
	ErrNilDeliverer = errors.New("retry: deliverer must not be nil")
	// ErrNilState indicates that the caller did not provide lifecycle state.
	ErrNilState = errors.New("retry: state must not be nil")
)

// Deliverer is the delivery operation boundary consumed by Coordinator.
// delivery.Engine satisfies this interface.
type Deliverer interface {
	Deliver(context.Context, delivery.Request) (delivery.Result, error)
}

// Outcome reports one coordinator invocation. DeliveryError is the normal
// delivery-layer error paired with Result; the error returned by Attempt is
// reserved for coordinator, configuration, context-preflight, or state errors.
type Outcome struct {
	Result        delivery.Result
	DeliveryError error
	Status        LifecycleStatus
	Schedule      *Schedule
}

// Coordinator performs exactly one delivery operation per Attempt call.
// It owns immutable dependencies and policy; callers own State and invocation
// timing. Coordinator never waits for or executes a returned Schedule.
type Coordinator struct {
	deliverer Deliverer
	backoff   BackoffPolicy
	limit     AttemptLimit
}

// NewCoordinator validates dependencies and policy before delivery can occur.
func NewCoordinator(deliverer Deliverer, backoff BackoffPolicy, limit AttemptLimit) (*Coordinator, error) {
	if deliverer == nil {
		return nil, ErrNilDeliverer
	}
	if _, err := backoff.Delay(1); err != nil {
		return nil, err
	}
	if _, err := Evaluate(nil, limit); err != nil {
		return nil, err
	}
	return &Coordinator{deliverer: deliverer, backoff: backoff, limit: limit}, nil
}

// Attempt performs exactly one delivery operation, records it, evaluates the
// lifecycle, and calculates scheduling metadata when another operation is
// allowed. The caller decides when to invoke Attempt and must enforce any
// previously returned NextRetryAt timestamp.
func (c *Coordinator) Attempt(ctx context.Context, state *State, req delivery.Request, now time.Time) (Outcome, error) {
	if state == nil {
		return Outcome{}, ErrNilState
	}

	status, err := Evaluate(state, c.limit)
	if err != nil {
		return Outcome{}, err
	}
	switch status {
	case StatusSucceeded, StatusFailed:
		return Outcome{Status: status}, ErrNotRetryable
	case StatusExhausted:
		return Outcome{Status: status}, ErrRetryExhausted
	}
	if err := ctx.Err(); err != nil {
		return Outcome{Status: status}, err
	}

	result, deliveryErr := c.deliverer.Deliver(ctx, req)
	outcome := Outcome{Result: result, DeliveryError: deliveryErr}
	if err := state.Record(result, deliveryErr); err != nil {
		return outcome, err
	}

	status, err = Evaluate(state, c.limit)
	if err != nil {
		return outcome, err
	}
	outcome.Status = status
	if status != StatusRetryable {
		return outcome, nil
	}

	schedule, err := NextSchedule(state, c.backoff, c.limit, now)
	if err != nil {
		return outcome, err
	}
	outcome.Schedule = &schedule
	return outcome, nil
}
