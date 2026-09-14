package retry

import (
	"errors"

	"github.com/Ferousco-dev/mailx/internal/delivery"
)

// ErrTerminalState is returned when a delivery operation is recorded after
// the retry lifecycle has already reached terminal success or failure.
var ErrTerminalState = errors.New("retry: lifecycle is terminal")

// DeliveryAttempt records one completed delivery operation in a retry
// lifecycle. Result.Attempts remains the lower-level history of MX/SMTP
// transfer attempts made within this single operation.
type DeliveryAttempt struct {
	Number       int
	Result       delivery.Result
	Decision     Decision
	ErrorMessage string
}

// State tracks completed delivery operations for one retry lifecycle.
// Its zero value is ready for use. State is intended to be owned and mutated
// by one retry coordinator; concurrent access requires external synchronization.
type State struct {
	deliveryAttempts []DeliveryAttempt
}

// Record adds one completed delivery operation and assigns the next one-based
// attempt number. It rejects additions after a terminal decision without
// changing existing history.
func (s *State) Record(result delivery.Result, err error) error {
	if len(s.deliveryAttempts) > 0 {
		latest := s.deliveryAttempts[len(s.deliveryAttempts)-1]
		if latest.Decision != Retry {
			return ErrTerminalState
		}
	}

	attempt := DeliveryAttempt{
		Number:   len(s.deliveryAttempts) + 1,
		Result:   cloneDeliveryResult(result),
		Decision: Decide(result, err),
	}
	if err != nil {
		attempt.ErrorMessage = err.Error()
	}
	s.deliveryAttempts = append(s.deliveryAttempts, attempt)
	return nil
}

// Count returns the number of completed delivery operations recorded.
func (s *State) Count() int {
	return len(s.deliveryAttempts)
}

// Latest returns a snapshot of the most recently recorded delivery operation.
func (s *State) Latest() (DeliveryAttempt, bool) {
	if len(s.deliveryAttempts) == 0 {
		return DeliveryAttempt{}, false
	}
	return cloneDeliveryAttempt(s.deliveryAttempts[len(s.deliveryAttempts)-1]), true
}

// History returns snapshots of all completed delivery operations in attempt
// number order. Mutating the returned slice does not alter State.
func (s *State) History() []DeliveryAttempt {
	history := make([]DeliveryAttempt, len(s.deliveryAttempts))
	for i, attempt := range s.deliveryAttempts {
		history[i] = cloneDeliveryAttempt(attempt)
	}
	return history
}

func cloneDeliveryAttempt(attempt DeliveryAttempt) DeliveryAttempt {
	attempt.Result = cloneDeliveryResult(attempt.Result)
	return attempt
}

func cloneDeliveryResult(result delivery.Result) delivery.Result {
	result.MXCandidates = append(result.MXCandidates[:0:0], result.MXCandidates...)
	result.Attempts = append(result.Attempts[:0:0], result.Attempts...)
	for i, attempt := range result.Attempts {
		if attempt.TransferErr != nil {
			transferErr := *attempt.TransferErr
			result.Attempts[i].TransferErr = &transferErr
		}
	}
	return result
}
