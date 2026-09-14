// Package delivery — same-operation MX fallback policy.
//
// RFC 5321 §5.1 says a sender SHOULD try each MX in preference order until
// delivery succeeds or the list is exhausted. In practice, once an SMTP
// conversation was successfully opened with a given MX and that server
// answered protocol commands, its answer is authoritative for the recipient
// domain — a second MX for the same domain will typically say the same
// thing (they're both operated by the destination). Trying a second MX in
// that case risks duplicate work and unusual DSNs.
//
// MailX therefore falls back to the next MX only when the failure was
// pre-session: unable to connect, greeting failure, or EHLO/HELO refused.
// Once past EHLO, the message-level outcome (temp or perm) is the final
// answer for this delivery operation.
package delivery

import (
	"errors"

	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/transfer"
)

// fallbackDecision indicates whether the delivery engine should attempt the
// next MX candidate after this one failed.
type fallbackDecision int

const (
	stopPermanent fallbackDecision = iota // definitive failure — do not try next MX, do not retry later
	stopTemporary                         // temporary failure — do not try next MX; v0.11 may retry later
	tryNext                               // pre-session failure — attempt next MX in this operation
)

// decideFallback classifies one attempt's outcome. err is the *TransferError
// returned by transfer.Service. Note: it is intentional that we do NOT stop
// on errors.Is(err, context.DeadlineExceeded) here — the SMTP client's inner
// dial timeout also produces that error, and a dial timeout on one MX
// should let the engine try the next MX. The caller's context is checked
// explicitly at the top of the Deliver loop instead.
func decideFallback(err error) fallbackDecision {
	var te *transfer.TransferError
	if !errors.As(err, &te) {
		// Unknown error shape — be conservative: treat as temporary stop.
		return stopTemporary
	}
	switch te.Stage {
	case smtp.StageInvalidInput:
		return stopPermanent
	case smtp.StageDial:
		// Never contacted this MX — try the next.
		return tryNext
	case smtp.StageGreeting, smtp.StageEHLO, smtp.StageHELO:
		// Pre-session failure (bad greeting, EHLO refused, malformed reply).
		// Try the next MX regardless of temp/perm — the server is arguably
		// unusable, and the next MX may be a different implementation.
		return tryNext
	case smtp.StageMailFrom, smtp.StageRcptTo, smtp.StageData, smtp.StageMessage, smtp.StageDataResponse:
		// The remote SMTP server engaged in the transaction and answered.
		// Its answer is authoritative for this domain.
		if te.Temporary {
			return stopTemporary
		}
		return stopPermanent
	case smtp.StageQuit:
		// QUIT-only failure after acceptance is handled separately — the
		// engine never reaches decideFallback in that case. If somehow it
		// does, treat as stopTemporary conservatively.
		return stopTemporary
	default:
		return stopTemporary
	}
}

// classifyFinal maps the terminating attempt's fallback decision into a
// Kind for the DeliveryResult / Error.
func classifyFinal(decision fallbackDecision, te *transfer.TransferError) Kind {
	switch decision {
	case stopPermanent:
		if te != nil && te.Stage == smtp.StageInvalidInput {
			return KindInvalidRequest
		}
		return KindTransferPermanent
	case stopTemporary:
		return KindTransferTemporary
	default:
		return KindTransferTemporary
	}
}
