// Package smtpidentity models MailX's PUBLIC SMTP infrastructure identity: the
// operator-configured hostname that MailX presents in EHLO/HELO, uses as the
// Message-ID domain and as the DSN Reporting-MTA, and the reverse/forward DNS
// that lets receivers confirm it (PTR and forward-confirmed reverse DNS).
//
// It is deliberately NOT a tenant concept. A tenant's verified domain governs
// From authorization, DKIM d=, SPF policy and DMARC alignment; the
// infrastructure hostname belongs to whoever operates the sending IPs and
// controls their PTR. The two are different identity layers and are never derived
// from one another (no "smtp.<tenant-domain>").
//
// Nothing here guarantees inbox placement, changes SPF/DKIM/DMARC semantics, or
// affects SMTP delivery truth. Live DNS checks are an operator diagnostic, not a
// startup or health dependency.
//
// Standards: RFC 5321 4.1.1.1 (EHLO carries the client's FQDN, else an address
// literal; EHLO/HELO MUST precede a transaction), RFC 7208 2.3-2.4 (HELO
// identity, and postmaster@HELO for a null reverse-path), RFC 1912 2.1 (PTR and
// A must match for every address), RFC 5322 3.6.4 (Message-ID right-hand side is
// a domain of the generating host). Gmail-style sender requirements (forward and
// reverse DNS, EHLO consistent with the PTR name) are receiver practice, not RFC
// requirements, and are reported as readiness, never enforced at startup.
package smtpidentity

import (
	"net"
	"strings"

	"golang.org/x/net/publicsuffix"

	"github.com/Ferousco-dev/mailx/internal/domain"
)

// LocalIdentity is the development-only identity used when no public hostname
// is configured. It is never valid as a public hostname.
const LocalIdentity = "mailx.local"

// MaxHostnameBytes is the DNS name limit.
const MaxHostnameBytes = 253

// HostnameError is a rejected hostname. Reason is a bounded code; the offending
// value is never included.
type HostnameError struct{ Reason string }

func (e *HostnameError) Error() string { return "invalid SMTP hostname: " + e.Reason }

// Rejection reasons.
const (
	ReasonEmpty          = "empty"
	ReasonTooLong        = "too_long"
	ReasonControlOrSpace = "control_or_whitespace"
	ReasonNotASCII       = "not_ascii"
	ReasonIPLiteral      = "ip_literal"
	ReasonURL            = "url_or_path"
	ReasonHostPort       = "host_with_port"
	ReasonEmailAddress   = "email_address"
	ReasonWildcard       = "wildcard"
	ReasonLocalName      = "local_or_internal_name"
	ReasonSingleLabel    = "single_label"
	ReasonPublicSuffix   = "public_suffix_only"
	ReasonInvalidLabel   = "invalid_label"
	ReasonIDN            = "idn_unsupported"
	ReasonNotPublicFQDN  = "not_a_public_fqdn"
)

// reservedTLDs are names that can never be public SMTP hostnames (RFC 6761/6762
// special-use names and common private-network suffixes).
var reservedTLDs = map[string]bool{
	"local": true, "localhost": true, "localdomain": true, "internal": true, "lan": true, "home": true,
	"corp": true, "private": true, "intranet": true, "invalid": true, "test": true, "example": true,
	"arpa": true, "onion": true,
}

// ValidateHostname checks that raw is a public, fully-qualified ASCII host name
// and returns its canonical form (lower case, one trailing root dot removed).
// It never repairs input: surrounding whitespace, URLs, ports, mailboxes,
// wildcards, IP literals, reserved and single-label names are all rejected. IDN
// (non-ASCII and xn-- labels) is rejected as everywhere else in MailX.
func ValidateHostname(raw string) (string, error) {
	if raw == "" {
		return "", &HostnameError{ReasonEmpty}
	}
	if len(raw) > MaxHostnameBytes+1 {
		return "", &HostnameError{ReasonTooLong}
	}
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; {
		case c <= 0x20 || c == 0x7f:
			return "", &HostnameError{ReasonControlOrSpace}
		case c >= 0x80:
			return "", &HostnameError{ReasonNotASCII}
		}
	}
	switch {
	case strings.ContainsAny(raw, "[]") || net.ParseIP(raw) != nil:
		return "", &HostnameError{ReasonIPLiteral}
	case strings.ContainsAny(raw, "/?#\\"):
		return "", &HostnameError{ReasonURL}
	case strings.Contains(raw, "@"):
		return "", &HostnameError{ReasonEmailAddress}
	case strings.Contains(raw, ":"):
		return "", &HostnameError{ReasonHostPort}
	case strings.Contains(raw, "*"):
		return "", &HostnameError{ReasonWildcard}
	}
	name := strings.ToLower(strings.TrimSuffix(raw, "."))
	if name == "" || len(name) > MaxHostnameBytes {
		return "", &HostnameError{ReasonInvalidLabel}
	}
	labels := strings.Split(name, ".")
	if reservedTLDs[labels[len(labels)-1]] || name == "localhost" {
		return "", &HostnameError{ReasonLocalName}
	}
	if len(labels) < 2 {
		return "", &HostnameError{ReasonSingleLabel}
	}
	if suffix, _ := publicsuffix.PublicSuffix(name); suffix == name {
		return "", &HostnameError{ReasonPublicSuffix}
	}
	for _, l := range labels {
		if strings.HasPrefix(l, "xn--") {
			return "", &HostnameError{ReasonIDN} // A-labels are unsupported, as for domain ownership
		}
		if !validHostLabel(l) {
			return "", &HostnameError{ReasonInvalidLabel}
		}
	}
	// The same canonicalization and public-suffix policy as domain ownership, so
	// MailX has one definition of "a public DNS name" (ICANN suffix, ASCII LDH).
	if got, err := domain.Normalize(name); err != nil || got != name {
		return "", &HostnameError{ReasonNotPublicFQDN}
	}
	return name, nil
}

// validHostLabel is RFC 1912 2.1 / RFC 1123: ASCII letters, digits and hyphen,
// beginning and ending with a letter or digit, at most 63 bytes.
func validHostLabel(l string) bool {
	if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}
