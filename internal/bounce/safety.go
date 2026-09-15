// Bounce safety: null reverse-path envelope construction and loop
// prevention (RFC 5321 §4.5.5, RFC 3464 §2.1). A DSN's own outbound
// envelope always uses the null reverse path, so a failed DSN can never
// trigger another DSN.
package bounce

import (
	"errors"
	"strings"

	"github.com/Ferousco-dev/mailx/internal/mail"
)

var (
	ErrOriginalSenderNull  = errors.New("bounce: original message reverse path is null — refusing to generate a DSN for a DSN")
	ErrOriginalSenderEmpty = errors.New("bounce: original sender is empty")
)

// IsNullReversePath reports whether mailFrom is "<>".
func IsNullReversePath(mailFrom string) bool {
	return strings.TrimSpace(mailFrom) == "<>"
}

// ShouldGenerate combines DSN eligibility with bounce-loop safety so a
// caller cannot skip either check independently.
func ShouldGenerate(originalMailFrom string, f Failure) (bool, error) {
	if IsNullReversePath(originalMailFrom) {
		return false, ErrOriginalSenderNull
	}
	if strings.TrimSpace(originalMailFrom) == "" {
		return false, ErrOriginalSenderEmpty
	}
	if !f.Eligible() {
		return false, ErrNotDSNEligible
	}
	return true, nil
}

// BounceEnvelope builds the null-reverse-path envelope MailX uses to send
// a DSN, with the original sender as sole recipient.
func BounceEnvelope(originalMailFrom string) (mail.Envelope, error) {
	if IsNullReversePath(originalMailFrom) {
		return mail.Envelope{}, ErrOriginalSenderNull
	}
	trimmed := strings.TrimSpace(originalMailFrom)
	if trimmed == "" {
		return mail.Envelope{}, ErrOriginalSenderEmpty
	}
	if !strings.HasPrefix(trimmed, "<") || !strings.HasSuffix(trimmed, ">") {
		return mail.Envelope{}, errors.New("bounce: original sender must be angle-bracketed")
	}
	if strings.ContainsAny(trimmed, "\r\n\x00") {
		return mail.Envelope{}, errors.New("bounce: original sender contains forbidden control character")
	}
	return mail.Envelope{
		MailFrom:   "<>",
		Recipients: []string{trimmed},
	}, nil
}
