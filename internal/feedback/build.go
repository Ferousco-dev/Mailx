package feedback

import (
	"crypto/sha256"
	"errors"
	"time"

	"github.com/Ferousco-dev/mailx/internal/bounce"
)

// ErrNoRecipient means a ParsedDSN group named no Final-Recipient — RFC 3464
// requires it (Original-Recipient is optional), so a group without one cannot
// be correlated to any of the message's recipients and is refused rather than
// guessed at.
var ErrNoRecipient = errors.New("feedback: delivery-status group has no Final-Recipient")

// FromDSN turns one already-classified ParsedDSN group into a Feedback ready
// for correlation. raw is the COMPLETE original submission (used only for the
// idempotency hash, never stored verbatim — see docs/design-v0.32.md
// "Retention"): the same bytes redelivered produce the same RawSHA256 for
// EVERY recipient group in a multi-recipient DSN, which is correct — the
// dedup key also includes recipient_id (see migration 000015), so redelivery
// is a no-op per recipient, not merged across recipients.
func FromDSN(d ParsedDSN, raw []byte, receivedAt time.Time) (Feedback, error) {
	if d.FinalRecipient == "" {
		return Feedback{}, ErrNoRecipient
	}
	kind, status := Classify(d)
	f := Feedback{
		Kind: kind, Source: SourceDSN, Action: d.Action,
		Diagnostic: d.DiagnosticCode, RemoteMTA: d.RemoteMTA, ReportingMTA: d.ReportingMTA,
		FinalRecipient: d.FinalRecipient, OriginalRecipient: d.OriginalRecipient,
		ReceivedAt: receivedAt.UTC(), RawSHA256: sha256.Sum256(raw),
	}
	if status != (bounce.EnhancedStatus{}) {
		f.EnhancedStatus = status.String()
	}
	return f, nil
}

// FromComplaint builds a Feedback for a verified complaint report (source =
// complaint; see docs/design-v0.32.md "Complaint feedback" for why there is no
// generic parser here: unlike DSNs, there is no single standardized complaint
// wire format, so the ingestion boundary is responsible for producing a
// Feedback directly from whatever its trusted source sends, and this
// constructor only fills in the fields every complaint needs).
func FromComplaint(recipient string, receivedAt time.Time, raw []byte) (Feedback, error) {
	if recipient == "" {
		return Feedback{}, ErrNoRecipient
	}
	return Feedback{
		Kind: KindComplaint, Source: SourceComplaint,
		FinalRecipient: recipient, ReceivedAt: receivedAt.UTC(), RawSHA256: sha256.Sum256(raw),
	}, nil
}

// Suppresses reports whether this Feedback's Kind, on its own, ever justifies
// automatic suppression — the coarse gate before the narrower enhanced-status
// check (suppression.QualifiesAsyncHardBounce) for bounce_permanent.
func (f Feedback) Suppresses() bool {
	return f.Kind == KindComplaint || f.Kind == KindBouncePermanent
}
