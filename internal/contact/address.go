// Package contact defines contact identity: the canonical key for "this
// tenant knows this recipient" (v0.34). Deliberately separate from
// suppression.Normalize — a suppression key is a deny-list, where folding
// case is the SAFE direction (over-blocking); a contact key identifies a
// real mailbox, where folding case risks merging two distinct people. Do not
// reuse this for suppression or vice versa.
package contact

import (
	"errors"
	"net/mail"
	"strings"
)

var ErrInvalidAddress = errors.New("contact: invalid email address")

const (
	maxAddressBytes = 254
	maxLocalBytes   = 64
)

// Normalize returns the canonical contact identity key.
//
// Rules:
//   - trim surrounding whitespace, strip one "<...>" bracket pair;
//   - ASCII only (MailX has no SMTPUTF8/IDN recipients anywhere);
//   - must parse as one RFC 5322 addr-spec, no display name/quoting/comments;
//   - domain is lower-cased and loses one trailing root dot (DNS/SMTP domains
//     are case-insensitive — folding here cannot merge two different mailboxes);
//   - LOCAL PART CASE IS PRESERVED, unlike suppression.Normalize: two addresses
//     differing only by local-part case are historically distinct mailboxes
//     under RFC 5321, and a contact should never silently merge two real
//     people. A tenant who wants "Alice@x.com" and "alice@x.com" treated as one
//     contact must say so explicitly (not v0.34's problem to solve).
//   - no provider-specific folding (no Gmail dot/plus-tag folding, no Outlook
//     aliasing): MailX has no basis to assume a specific provider.
func Normalize(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if len(s) >= 2 && s[0] == '<' && s[len(s)-1] == '>' {
		s = s[1 : len(s)-1]
	}
	if strings.HasSuffix(s, ".") {
		s = s[:len(s)-1]
	}
	if s == "" || len(s) > maxAddressBytes {
		return "", ErrInvalidAddress
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= 0x20 || c >= 0x7f {
			return "", ErrInvalidAddress
		}
	}
	parsed, err := mail.ParseAddress(s)
	if err != nil || parsed.Name != "" || parsed.Address != s {
		return "", ErrInvalidAddress
	}
	at := strings.LastIndexByte(s, '@')
	if at < 0 {
		return "", ErrInvalidAddress
	}
	local, domain := s[:at], s[at+1:]
	if local == "" || len(local) > maxLocalBytes || domain == "" {
		return "", ErrInvalidAddress
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || !isLDH(label) {
			return "", ErrInvalidAddress
		}
	}
	return local + "@" + strings.ToLower(domain), nil
}

func isLDH(label string) bool {
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	allDigits := true
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= '0' && c <= '9':
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '-':
			allDigits = false
		default:
			return false
		}
	}
	return !allDigits // reject IP-literal-looking all-numeric domains
}
