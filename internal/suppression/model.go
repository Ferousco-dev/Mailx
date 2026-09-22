// Package suppression defines MailX's recipient suppression policy: the durable
// statement "this tenant must not send ordinary mail to this address".
//
// Suppression is a SAFETY POLICY, deliberately separate from delivery history: a
// message that failed or bounced stays exactly what happened; a suppression is a
// consequence that governs FUTURE sends. Removing a suppression never rewrites,
// retries or resurrects anything.
//
// This package is pure (no I/O): the vocabulary, the ONE address normalization used
// by the API, the worker and the store, and the rule that decides which delivery
// failures justify automatic suppression. Persistence lives in internal/database.
package suppression

import "errors"

// Reason says WHY a recipient is suppressed.
type Reason string

const (
	ReasonManual      Reason = "manual"      // an operator/developer asked for it
	ReasonHardBounce  Reason = "hard_bounce" // the recipient address is permanently undeliverable
	ReasonComplaint   Reason = "complaint"   // reserved: needs feedback-loop ingestion (v0.32); nothing creates it yet
	ReasonUnsubscribe Reason = "unsubscribe" // reserved: needs a subscription product; nothing creates it yet
)

// Source says HOW the entry was created (independent of the reason).
type Source string

const (
	SourceAPI      Source = "api"      // POST /v1/suppressions
	SourceDelivery Source = "delivery" // the worker, from a delivery outcome
	SourceFeedback Source = "feedback" // reserved for future trusted complaint/feedback ingestion
)

// ErrInvalidReason is returned for a reason the caller may not use.
var ErrInvalidReason = errors.New("suppression: invalid reason")

// ParseAPIReason validates a reason supplied through the public API. Only
// "manual" may be created by a caller: hard bounces come from delivery outcomes,
// and complaint/unsubscribe have no implemented producer, so accepting them from
// the API would let a caller forge system-derived facts. Empty means manual.
func ParseAPIReason(s string) (Reason, error) {
	switch s {
	case "", string(ReasonManual):
		return ReasonManual, nil
	}
	return "", ErrInvalidReason
}

// Valid reports whether r is in the stored vocabulary.
func (r Reason) Valid() bool {
	switch r {
	case ReasonManual, ReasonHardBounce, ReasonComplaint, ReasonUnsubscribe:
		return true
	}
	return false
}

// Valid reports whether s is in the stored vocabulary.
func (s Source) Valid() bool {
	switch s {
	case SourceAPI, SourceDelivery, SourceFeedback:
		return true
	}
	return false
}

// HardBounceStage is the SMTP stage at which a RCPT TO rejection happens.
const HardBounceStage = "rcpt_to"

// QualifiesHardBounce decides whether a FAILED delivery attempt justifies
// automatically suppressing the recipient it names.
//
// It is intentionally narrow. A permanent SMTP failure is not automatically a
// statement about the recipient: it may concern the sender, policy, content,
// authentication or the sending IP. Only a permanent rejection of the RCPT TO
// command itself, carrying an enhanced status code that RFC 3463 defines as being
// about the destination mailbox, qualifies:
//
//	5.1.1  Bad destination mailbox address   (the mailbox does not exist)
//	5.1.6  Destination mailbox has moved, no forwarding address
//
// Everything else does NOT qualify: temporary (4xx) failures, other 5.1.x codes
// (5.1.2 and 5.1.10 concern the domain, 5.1.3 is syntax), 5.2.x (a mailbox that is
// full or disabled can recover), 5.7.x (policy/authentication), failures at any
// stage other than RCPT TO, and a bare 550 with no enhanced code (too ambiguous:
// it is also used for blocklist and policy rejections). Uses structured
// fields only; never message text.
func QualifiesHardBounce(stage string, permanent bool, accepted bool, code int, enhanced, recipient string) bool {
	if !permanent || accepted || recipient == "" || stage != HardBounceStage || code < 500 || code > 599 {
		return false
	}
	switch enhanced {
	case "5.1.1", "5.1.6":
		return true
	}
	return false
}

// hardBounceEnhancedStatuses is the narrow set of RFC 3463 enhanced-status
// codes that, on their own, prove the DESTINATION MAILBOX itself is invalid —
// shared by QualifiesHardBounce (synchronous RCPT TO rejection) and
// QualifiesAsyncHardBounce (v0.32 asynchronous DSN feedback) so the two paths
// can never silently diverge on what "hard bounce" means.
var hardBounceEnhancedStatuses = map[string]bool{"5.1.1": true, "5.1.6": true}

// QualifiesAsyncHardBounce decides whether a PERMANENT asynchronous bounce
// (an RFC 3464 DSN with Action: failed) justifies automatic suppression.
//
// Deliberately as narrow as QualifiesHardBounce, and for the same reason: a
// permanent DSN is not automatically a statement about the recipient mailbox —
// it may concern policy, content, authentication, or the sending
// reputation/IP, none of which the recipient address itself caused. Only the
// same two mailbox-invalidity enhanced-status codes qualify. enhanced must
// already be validated (bounce.ParseEnhancedStatus) and confirmed class 5 by
// the caller; this function re-checks the canonical string form so a caller
// cannot accidentally suppress on an unvalidated value.
func QualifiesAsyncHardBounce(enhanced string) bool {
	return hardBounceEnhancedStatuses[enhanced]
}
