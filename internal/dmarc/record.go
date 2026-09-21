// Package dmarc gives a tenant sender-side DMARC readiness for a domain MailX
// sends from, following RFC 9989 (which obsoletes RFC 7489): it inspects the
// published DMARC policy record, models how MailX's DKIM and SPF identities
// align with the RFC 5322 From domain, and recommends a record.
//
// It is NOT a receiving-side DMARC engine. It never evaluates a message, never
// generates Authentication-Results, DMARC-Signature or any other header, never
// blocks or authorizes sending, and never claims a receiver's DMARC result:
// "ready" means the DNS policy exists and at least one aligned authentication
// path is configured, which is a precondition for a receiver to pass DMARC, not
// proof that it did. DNS is the source of truth; nothing here is persisted.
//
// Bounds (untrusted DNS): 5 s per verification, 16 concurrent verifications,
// at most 8 DNS queries, 64 TXT records per answer, 2048-byte record, 32 tags,
// 16 report URIs. No regular expressions, no recursion.
package dmarc

import (
	"net/mail"
	"strings"
)

// Parse bounds.
const (
	MaxRecordBytes = 2048
	MaxTags        = 32
	MaxTagValue    = 1024
	MaxReportURIs  = 16
	MaxTXTRecords  = 64
)

// Policy is the requested handling for mail that fails DMARC (p, sp, np).
type Policy string

const (
	PolicyNone       Policy = "none"
	PolicyQuarantine Policy = "quarantine"
	PolicyReject     Policy = "reject"
)

// Mode is an alignment mode (adkim, aspf).
type Mode string

const (
	Relaxed Mode = "r"
	Strict  Mode = "s"
)

// InvalidError is a malformed DMARC record; Reason is a bounded code, never
// record text.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return "dmarc: invalid record: " + e.Reason }

// Invalid reason codes.
const (
	ReasonTooLong       = "record_too_long"
	ReasonBadEncoding   = "bad_encoding"
	ReasonBadVersion    = "bad_version"
	ReasonBadTag        = "bad_tag"
	ReasonTooManyTags   = "too_many_tags"
	ReasonTagTooLong    = "tag_value_too_long"
	ReasonDuplicateTag  = "duplicate_tag"
	ReasonBadPolicy     = "bad_policy"
	ReasonBadAlignment  = "bad_alignment"
	ReasonBadValue      = "bad_value"
	ReasonMissingPolicy = "missing_policy"
	ReasonTooManyURIs   = "too_many_report_uris"
)

// Warning codes shared by parsing and analysis.
const (
	WarnDeprecatedTag      = "deprecated_tag"
	WarnIgnoredReportURI   = "ignored_report_uri"
	WarnPolicyFromRUA      = "policy_defaulted_from_rua"
	WarnMonitoringOnly     = "policy_not_enforcing"
	WarnTesting            = "testing_mode"
	WarnFailureReporting   = "failure_reporting_enabled"
	WarnExternalReportDest = "external_report_destination"
	WarnRelayAlters        = "relay_may_alter_signed_content"
)

// Record is a parsed DMARC policy record. Only tags MailX understands are kept.
type Record struct {
	Policy    Policy // p (defaulted to none only when a valid rua exists)
	SubPolicy Policy // sp; empty when absent
	NonExist  Policy // np; empty when absent
	Adkim     Mode   // default relaxed
	Aspf      Mode   // default relaxed
	Testing   bool   // t=y
	PSD       string // y, n or u (default)
	RUAHosts  []string
	RUFCount  int
	Warnings  []string
}

var knownTags = map[string]bool{"v": true, "p": true, "sp": true, "np": true, "adkim": true, "aspf": true,
	"t": true, "psd": true, "rua": true, "ruf": true, "fo": true, "pct": true, "rf": true, "ri": true}

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

// IsDMARC reports whether a TXT value is a DMARC record: its first tag is the
// version tag with value DMARC1 (RFC 9989: records not starting with a valid
// version tag are discarded). Tag names and values are matched
// case-insensitively, the lenient reading of the ABNF.
func IsDMARC(txt string) bool {
	first, _, _ := strings.Cut(txt, ";")
	name, val, ok := strings.Cut(first, "=")
	return ok && strings.EqualFold(trimWSP(name), "v") && strings.EqualFold(trimWSP(val), "DMARC1")
}

// Parse parses one DMARC record. Unknown tags are ignored (RFC 9989). Known
// tags that are duplicated or carry invalid values make the record invalid for
// MailX's purposes even where receivers might default them, because the owner's
// intent would be ambiguous.
func Parse(txt string) (Record, error) {
	if len(txt) > MaxRecordBytes {
		return Record{}, &InvalidError{ReasonTooLong}
	}
	for i := 0; i < len(txt); i++ {
		if txt[i] < 0x20 && txt[i] != '\t' || txt[i] >= 0x7f {
			return Record{}, &InvalidError{ReasonBadEncoding}
		}
	}
	rec := Record{Adkim: Relaxed, Aspf: Relaxed, PSD: "u"}
	seen := map[string]bool{}
	n := 0
	var rua string
	var hasRUA bool
	for _, part := range strings.Split(txt, ";") {
		part = trimWSP(part)
		if part == "" {
			continue // trailing or doubled ';' is tolerated
		}
		if n++; n > MaxTags {
			return Record{}, &InvalidError{ReasonTooManyTags}
		}
		name, val, ok := strings.Cut(part, "=")
		name, val = strings.ToLower(trimWSP(name)), trimWSP(val)
		if !ok || name == "" || len(name) > 32 {
			return Record{}, &InvalidError{ReasonBadTag}
		}
		if len(val) > MaxTagValue {
			return Record{}, &InvalidError{ReasonTagTooLong}
		}
		if n == 1 && (name != "v" || !strings.EqualFold(val, "DMARC1")) {
			return Record{}, &InvalidError{ReasonBadVersion}
		}
		if !knownTags[name] {
			continue
		}
		if seen[name] {
			return Record{}, &InvalidError{ReasonDuplicateTag}
		}
		seen[name] = true
		lv := strings.ToLower(val)
		switch name {
		case "p", "sp", "np":
			pol, ok := toPolicy(lv)
			if !ok {
				return Record{}, &InvalidError{ReasonBadPolicy}
			}
			switch name {
			case "p":
				rec.Policy = pol
			case "sp":
				rec.SubPolicy = pol
			default:
				rec.NonExist = pol
			}
		case "adkim", "aspf":
			if lv != "r" && lv != "s" {
				return Record{}, &InvalidError{ReasonBadAlignment}
			}
			if name == "adkim" {
				rec.Adkim = Mode(lv)
			} else {
				rec.Aspf = Mode(lv)
			}
		case "t":
			if lv != "y" && lv != "n" {
				return Record{}, &InvalidError{ReasonBadValue}
			}
			rec.Testing = lv == "y"
		case "psd":
			if lv != "y" && lv != "n" && lv != "u" {
				return Record{}, &InvalidError{ReasonBadValue}
			}
			rec.PSD = lv
		case "rua":
			rua, hasRUA = val, true
		case "ruf":
			c, _, err := reportURIs(val, &rec)
			if err != nil {
				return Record{}, err
			}
			rec.RUFCount = c
		case "pct", "rf", "ri":
			rec.Warnings = appendOnce(rec.Warnings, WarnDeprecatedTag) // removed by RFC 9989; ignored
		}
	}
	if n == 0 || !seen["v"] {
		return Record{}, &InvalidError{ReasonBadVersion}
	}
	if hasRUA {
		_, hosts, err := reportURIs(rua, &rec)
		if err != nil {
			return Record{}, err
		}
		rec.RUAHosts = hosts
	}
	if rec.Policy == "" {
		if len(rec.RUAHosts) == 0 {
			return Record{}, &InvalidError{ReasonMissingPolicy}
		}
		rec.Policy = PolicyNone // RFC 9989: p absent with a valid rua acts as p=none
		rec.Warnings = appendOnce(rec.Warnings, WarnPolicyFromRUA)
	}
	if rec.RUFCount > 0 {
		rec.Warnings = appendOnce(rec.Warnings, WarnFailureReporting)
	}
	return rec, nil
}

func toPolicy(s string) (Policy, bool) {
	switch Policy(s) {
	case PolicyNone, PolicyQuarantine, PolicyReject:
		return Policy(s), true
	}
	return "", false
}

func appendOnce(ws []string, w string) []string {
	for _, x := range ws {
		if x == w {
			return ws
		}
	}
	return append(ws, w)
}

// reportURIs validates a rua/ruf list. Only mailto: URIs count (RFC 9989: other
// schemes are ignored); unusable entries only warn. It returns the number of
// usable URIs and their lower-case host names.
func reportURIs(list string, rec *Record) (int, []string, error) {
	var hosts []string
	entries := strings.Split(list, ",")
	if len(entries) > MaxReportURIs {
		return 0, nil, &InvalidError{ReasonTooManyURIs}
	}
	for _, e := range entries {
		e = trimWSP(e)
		if len(e) < 8 || !strings.EqualFold(e[:7], "mailto:") {
			rec.Warnings = appendOnce(rec.Warnings, WarnIgnoredReportURI)
			continue
		}
		addr, _, _ := strings.Cut(e[7:], "!") // optional size limit suffix
		if len(addr) > 254 {
			rec.Warnings = appendOnce(rec.Warnings, WarnIgnoredReportURI)
			continue
		}
		parsed, err := mail.ParseAddress(addr)
		at := -1
		if err == nil {
			at = strings.LastIndexByte(parsed.Address, '@')
		}
		if err != nil || at <= 0 || at == len(parsed.Address)-1 {
			rec.Warnings = appendOnce(rec.Warnings, WarnIgnoredReportURI)
			continue
		}
		hosts = append(hosts, strings.ToLower(parsed.Address[at+1:]))
	}
	return len(hosts), hosts, nil
}
