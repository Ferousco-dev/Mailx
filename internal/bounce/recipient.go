package bounce

import (
	"github.com/Ferousco-dev/mailx/internal/delivery"
)

// RecipientAction is restricted to the RFC 3464 §2.3.3 values MailX can
// truthfully produce today: "failed" and "delivered".
type RecipientAction uint8

const (
	ActionUnknown RecipientAction = iota
	ActionFailed
	ActionDelivered
)

func (a RecipientAction) String() string {
	switch a {
	case ActionFailed:
		return "failed"
	case ActionDelivered:
		return "delivered"
	default:
		return "unknown"
	}
}

// RecipientStatus is MailX's per-recipient view of one delivery outcome.
// Because outbound RCPT is all-or-error, a sibling recipient never
// individually confirmed is reported Failed with a generic diagnostic,
// never with another recipient's specific code or reason.
type RecipientStatus struct {
	FinalRecipient string
	Action         RecipientAction
	Status         *EnhancedStatus
	SMTPCode       int
	Diagnostic     string
}

const abortedDiagnostic = "transaction aborted before this recipient's RCPT TO could be confirmed or refused"

// RecipientStatuses derives one status per envelope recipient, never
// fabricating success or a diagnostic MailX did not observe.
func RecipientStatuses(f Failure, envelopeRecipients []string) []RecipientStatus {
	statuses := make([]RecipientStatus, 0, len(envelopeRecipients))
	for _, r := range envelopeRecipients {
		statuses = append(statuses, recipientStatusFor(f, r))
	}
	return statuses
}

func recipientStatusFor(f Failure, recipient string) RecipientStatus {
	if f.Recipient != "" && f.Recipient == recipient {
		return RecipientStatus{
			FinalRecipient: recipient,
			Action:         ActionFailed,
			Status:         f.EnhancedStatus,
			SMTPCode:       f.SMTPCode,
			Diagnostic:     f.RemoteMessage,
		}
	}
	if f.Recipient != "" {
		return RecipientStatus{
			FinalRecipient: recipient,
			Action:         ActionFailed,
			Diagnostic:     abortedDiagnostic,
		}
	}
	return RecipientStatus{
		FinalRecipient: recipient,
		Action:         ActionFailed,
		Status:         f.EnhancedStatus,
		SMTPCode:       f.SMTPCode,
		Diagnostic:     f.RemoteMessage,
	}
}

// DeliveredRecipientStatuses reports every recipient as delivered; callers
// must only use this when result.Accepted is true.
func DeliveredRecipientStatuses(envelopeRecipients []string, result delivery.Result) []RecipientStatus {
	statuses := make([]RecipientStatus, 0, len(envelopeRecipients))
	for _, r := range envelopeRecipients {
		statuses = append(statuses, RecipientStatus{
			FinalRecipient: r,
			Action:         ActionDelivered,
			SMTPCode:       result.FinalCode,
			Diagnostic:     result.RemoteMessage,
		})
	}
	return statuses
}
