// Package retry classifies completed delivery operations for delayed retry.
// It does not schedule, wait, or execute delivery attempts.
package retry

import (
	"context"
	"errors"

	"github.com/Ferousco-dev/mailx/internal/delivery"
)

// Decision describes what should happen after one delivery operation.
type Decision uint8

const (
	// TerminalFailure means the operation must not be retried automatically.
	// It is the zero value so incomplete or unknown outcomes fail safely.
	TerminalFailure Decision = iota
	// Retry means a later retry may be scheduled by a future retry executor.
	Retry
	// TerminalSuccess means the remote server accepted the message.
	TerminalSuccess
)

// Decide classifies one completed delivery operation.
//
// Acceptance is authoritative even when err reports a later failure, because
// retrying after the final DATA response may duplicate the message. Caller
// cancellation and deadline expiry are terminal for this operation; autonomous
// retry must not override the caller's context.
func Decide(result delivery.Result, err error) Decision {
	if result.Accepted {
		return TerminalSuccess
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return TerminalFailure
	}

	switch result.Kind {
	case delivery.KindDNSTemporary, delivery.KindTransferTemporary:
		return Retry
	case delivery.KindAccepted,
		delivery.KindInvalidRequest,
		delivery.KindDNSNotFound,
		delivery.KindDNSNullMX,
		delivery.KindDNSFailure,
		delivery.KindTransferPermanent,
		delivery.KindContext:
		return TerminalFailure
	default:
		return TerminalFailure
	}
}
