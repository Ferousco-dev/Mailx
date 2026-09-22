// Package bimi gives a tenant sender-side BIMI (Brand Indicators for Message
// Identification) readiness for a domain MailX sends from: it inspects the
// published BIMI Assertion Record, checks the DMARC prerequisite (consumed as
// an existing fact from internal/dmarc, never re-evaluated here), and, when
// asked, fetches and structurally validates the referenced logo (SVG) and
// authority evidence (VMC/CMC certificate).
//
// It is NOT a receiving-side BIMI engine and it makes NO claim about how any
// mailbox provider will actually render a logo. "ready" means MailX's own
// checks (DNS record, DMARC prerequisite, and — only when asset validation
// was requested — logo/certificate structure) all passed; it is a
// precondition mailbox providers may consult, never a display guarantee.
// Every Result carries an explicit Disclaimer saying so. See the BIMI
// Internet-Draft (draft-brand-indicators-for-message-identification) and the
// BIMI Group's SVG Tiny P/S guidance for the standards this follows.
//
// DNS is the source of truth; nothing here is persisted (same posture as
// internal/dmarc and internal/spf).
//
// Bounds (untrusted DNS/HTTP): the TXT record, the fetched SVG and the
// fetched certificate are each size- and time-bounded; no regular
// expressions, no recursion, no script/external-reference execution.
package bimi

import "strings"

// Parse bounds for the DNS TXT record.
const (
	MaxRecordBytes = 2048
	MaxTagValue    = 2048 // l=/a= are URLs; RFC 9989-style records use no smaller bound elsewhere in this codebase
)

// DefaultSelector is the only selector MailX checks. The BIMI draft also
// defines a per-message BIMI-Selector header mechanism for publishers who
// want several concurrent logos; MailX does not add BIMI headers to
// outbound mail and has no per-message selector concept to check against,
// so only the domain-wide default selector's readiness is assessed
// (deferred, not silently ignored — see .ilana/architecture.md).
const DefaultSelector = "default"

// bimiLabel is the DNS label prefix; the full query name is
// "<selector>._bimi.<domain>".
const bimiLabel = "._bimi."

// Record is a parsed BIMI Assertion Record.
type Record struct {
	// Location is the l= value: the HTTPS URL of the logo (SVG), or empty.
	Location string
	// Declined is true when l= was published with an explicitly empty
	// value — the domain owner's deliberate statement that no logo is
	// published, distinct from "no record at all" (not_configured).
	Declined bool
	// Authority is the a= value: the HTTPS URL of the Mark Certificate
	// (VMC/CMC), or empty when the tag is absent or itself empty.
	Authority string
}

// InvalidError is a malformed BIMI record; Reason is a bounded code, never
// record text.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return "bimi: invalid record: " + e.Reason }

const (
	ReasonTooLong    = "record_too_long"
	ReasonBadVersion = "bad_version"
	ReasonMissingL   = "missing_location_tag"
	ReasonBadURL     = "bad_url"
	ReasonDuplicate  = "duplicate_tag"
	ReasonTagTooLong = "tag_value_too_long"
	ReasonNotHTTPS   = "url_not_https"
)

func isWSP(c byte) bool { return c == ' ' || c == '\t' }

func trimWSP(s string) string {
	for len(s) > 0 && isWSP(s[0]) {
		s = s[1:]
	}
	for len(s) > 0 && isWSP(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return s
}

// IsBIMI reports whether a TXT value is a BIMI record: its first tag is the
// version tag with value BIMI1. Matched case-insensitively, the same
// lenient reading this codebase's dmarc.IsDMARC already uses for v=DMARC1.
func IsBIMI(txt string) bool {
	first, _, _ := strings.Cut(txt, ";")
	name, val, ok := strings.Cut(first, "=")
	return ok && strings.EqualFold(trimWSP(name), "v") && strings.EqualFold(trimWSP(val), "BIMI1")
}

// Parse parses one BIMI Assertion Record. l= is required by the ABNF (it may
// be empty, meaning declined); a= is optional. Unknown tags are ignored,
// mirroring this codebase's DMARC parser's lenient-unknown-tag stance.
func Parse(txt string) (Record, error) {
	if len(txt) > MaxRecordBytes {
		return Record{}, &InvalidError{Reason: ReasonTooLong}
	}
	if !IsBIMI(txt) {
		return Record{}, &InvalidError{Reason: ReasonBadVersion}
	}
	var rec Record
	haveL := false
	haveA := false
	for _, part := range strings.Split(txt, ";") {
		part = trimWSP(part)
		if part == "" {
			continue
		}
		name, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		name = trimWSP(name)
		val = trimWSP(val)
		if len(val) > MaxTagValue {
			return Record{}, &InvalidError{Reason: ReasonTagTooLong}
		}
		switch name {
		case "v":
			// already validated by IsBIMI; skip (first tag)
		case "l":
			if haveL {
				return Record{}, &InvalidError{Reason: ReasonDuplicate}
			}
			haveL = true
			if val == "" {
				rec.Declined = true
				continue
			}
			if !isHTTPSURL(val) {
				return Record{}, &InvalidError{Reason: ReasonNotHTTPS}
			}
			rec.Location = val
		case "a":
			if haveA {
				return Record{}, &InvalidError{Reason: ReasonDuplicate}
			}
			haveA = true
			if val == "" {
				continue
			}
			if !isHTTPSURL(val) {
				return Record{}, &InvalidError{Reason: ReasonNotHTTPS}
			}
			rec.Authority = val
		default:
			// unknown tag: ignored, per the draft's forward-compatibility stance
		}
	}
	if !haveL {
		return Record{}, &InvalidError{Reason: ReasonMissingL}
	}
	return rec, nil
}

func isHTTPSURL(s string) bool {
	return strings.HasPrefix(s, "https://") && len(s) > len("https://")
}

// QueryName returns the DNS TXT query name for selector at domain, e.g.
// "default._bimi.example.com".
func QueryName(selector, domain string) string {
	return selector + bimiLabel + domain
}
