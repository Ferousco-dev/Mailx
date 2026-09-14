// Package bounce classifies final MailX delivery failures for future DSN
// handling. It does not generate, send, or persist bounce messages.
package bounce

import (
	"errors"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/retry"
)

var (
	// ErrNilState indicates that no retry lifecycle was supplied.
	ErrNilState = errors.New("bounce: retry state must not be nil")
	// ErrNoDeliveryAttempts indicates that the lifecycle has no final result.
	ErrNoDeliveryAttempts = errors.New("bounce: no delivery attempts")
	// ErrNotFinal indicates that delivery remains eligible for a later retry.
	ErrNotFinal = errors.New("bounce: delivery lifecycle is not final")
	// ErrDeliverySucceeded prevents accepted mail from becoming a bounce.
	ErrDeliverySucceeded = errors.New("bounce: delivery succeeded")
	// ErrInvalidLifecycle indicates that status and recorded state disagree.
	ErrInvalidLifecycle = errors.New("bounce: inconsistent lifecycle state")
)

// FailureClass describes a broad, truthful final outcome. It deliberately
// avoids inferring mailbox, user, or policy details that MailX does not know.
type FailureClass uint8

const (
	// FailureUnknown is a terminal failure MailX cannot safely classify more
	// precisely. It is the zero value so new or malformed inputs fail safely.
	FailureUnknown FailureClass = iota
	// FailurePermanentDelivery is a permanent SMTP/transfer failure.
	FailurePermanentDelivery
	// FailureRetryExhausted means a temporary protocol failure became final
	// only because MailX reached its retry-operation limit.
	FailureRetryExhausted
	// FailureDNS is a permanent or otherwise terminal DNS destination failure.
	FailureDNS
	// FailureNullMX means the domain explicitly declares that it accepts no mail.
	FailureNullMX
	// FailureInvalidRequest is a local request/validation failure.
	FailureInvalidRequest
	// FailureAborted is a local caller cancellation or deadline termination.
	FailureAborted
)

// Failure is a read-only final-failure view derived from the latest completed
// delivery operation. It preserves existing diagnostics without copying the
// complete delivery result or inventing recipient-level meaning.
type Failure struct {
	Class             FailureClass
	DeliveryKind      delivery.Kind
	SMTPCode          int
	EnhancedStatusRaw string
	EnhancedStatus    *EnhancedStatus
	Message           string
	RemoteMessage     string
	FailureStage      string
	Recipient         string
	RetryExhausted    bool
	AttemptCount      int
}

// Classify produces a final-failure view from caller-owned retry state. The
// supplied lifecycle status must describe the same latest attempt. Classify
// never mutates state and refuses successful or still-retryable lifecycles.
func Classify(state *retry.State, status retry.LifecycleStatus) (Failure, error) {
	if state == nil {
		return Failure{}, ErrNilState
	}
	latest, ok := state.Latest()
	if !ok {
		if status != retry.StatusEmpty {
			return Failure{}, ErrInvalidLifecycle
		}
		return Failure{}, ErrNoDeliveryAttempts
	}

	switch status {
	case retry.StatusEmpty:
		return Failure{}, ErrInvalidLifecycle
	case retry.StatusRetryable:
		if latest.Decision != retry.Retry || latest.Result.Accepted {
			return Failure{}, ErrInvalidLifecycle
		}
		return Failure{}, ErrNotFinal
	case retry.StatusSucceeded:
		if latest.Decision != retry.TerminalSuccess || !latest.Result.Accepted {
			return Failure{}, ErrInvalidLifecycle
		}
		return Failure{}, ErrDeliverySucceeded
	case retry.StatusFailed:
		if latest.Decision != retry.TerminalFailure || latest.Result.Accepted {
			return Failure{}, ErrInvalidLifecycle
		}
		return failureFromAttempt(latest, state.Count(), false), nil
	case retry.StatusExhausted:
		if latest.Decision != retry.Retry || latest.Result.Accepted {
			return Failure{}, ErrInvalidLifecycle
		}
		return failureFromAttempt(latest, state.Count(), true), nil
	default:
		return Failure{}, ErrInvalidLifecycle
	}
}

func failureFromAttempt(attempt retry.DeliveryAttempt, count int, exhausted bool) Failure {
	result := attempt.Result
	message := result.FailureMessage
	if message == "" {
		message = attempt.ErrorMessage
	}
	var enhanced *EnhancedStatus
	if parsed, err := ParseEnhancedStatus(result.EnhancedStatus); err == nil {
		enhanced = &parsed
	}
	failure := Failure{
		Class:             classifyKind(result.Kind),
		DeliveryKind:      result.Kind,
		SMTPCode:          result.FinalCode,
		EnhancedStatusRaw: result.EnhancedStatus,
		EnhancedStatus:    enhanced,
		Message:           message,
		RemoteMessage:     result.RemoteMessage,
		FailureStage:      result.FailureStage,
		Recipient:         result.Recipient,
		AttemptCount:      count,
	}
	if exhausted {
		failure.Class = FailureRetryExhausted
		failure.RetryExhausted = true
	}
	return failure
}

func classifyKind(kind delivery.Kind) FailureClass {
	switch kind {
	case delivery.KindTransferPermanent:
		return FailurePermanentDelivery
	case delivery.KindDNSNotFound, delivery.KindDNSFailure:
		return FailureDNS
	case delivery.KindDNSNullMX:
		return FailureNullMX
	case delivery.KindInvalidRequest:
		return FailureInvalidRequest
	case delivery.KindContext:
		return FailureAborted
	default:
		return FailureUnknown
	}
}
