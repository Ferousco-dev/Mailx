package bounce

import (
	"errors"
	"strings"
	"time"
)

var ErrNotDSNEligible = errors.New("bounce: failure is not DSN-eligible")

// DSN is MailX's internal RFC 3464-shaped delivery status notification.
// Fields RFC 3464 defines but MailX cannot truthfully populate (Remote-MTA,
// Last-Attempt-Date, Original-Recipient) are omitted rather than guessed.
type DSN struct {
	ReportingMTA       string
	ArrivalDate        time.Time
	OriginalEnvelopeID string
	Recipients         []RecipientStatus
	Failure            Failure
}

// Eligible is the conservative decision of whether a failure may ever
// produce a DSN: permanent delivery failure, retry exhaustion, DNS
// failure, and Null MX are eligible; local/aborted/unknown failures are not.
func (f Failure) Eligible() bool {
	switch f.Class {
	case FailurePermanentDelivery, FailureRetryExhausted, FailureDNS, FailureNullMX:
		return true
	default:
		return false
	}
}

// NewDSN builds a DSN from a final Failure, refusing ineligible failures.
func NewDSN(f Failure, envelopeRecipients []string, reportingMTA string, deliveryID string, arrivalDate time.Time) (DSN, error) {
	if !f.Eligible() {
		return DSN{}, ErrNotDSNEligible
	}
	if strings.TrimSpace(reportingMTA) == "" {
		return DSN{}, errors.New("bounce: reporting MTA is empty")
	}
	if len(envelopeRecipients) == 0 {
		return DSN{}, errors.New("bounce: no recipients to report")
	}
	return DSN{
		ReportingMTA:       reportingMTA,
		ArrivalDate:        arrivalDate,
		OriginalEnvelopeID: deliveryID,
		Recipients:         RecipientStatuses(f, envelopeRecipients),
		Failure:            f,
	}, nil
}
