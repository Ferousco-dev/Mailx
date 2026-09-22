// Package spf helps a tenant publish a correct SPF record (RFC 7208) for a
// domain MailX sends from. It is NOT a receiving-side SPF engine: it never
// evaluates check_host() for a message, never generates Received-SPF or
// Authentication-Results headers, and never authorizes anything.
//
// What it does: parse a published record with strict bounds, decide whether the
// record literally authorizes MailX's configured sending infrastructure
// (ip4/ip6 mechanisms in direct mode; a named include in relay mode), and build
// a truthful record to publish or merge.
//
// Deliberate limits (documented in docs/design-v0.27.md):
//   - include, a, mx, exists, ptr and redirect are parsed but NEVER resolved.
//     RFC 7208 section 4.6.4 caps evaluation at 10 DNS-querying terms; walking a
//     tenant-controlled DNS tree is unbounded work, so MailX validates only the
//     literal subset and says so (Result.Unevaluated).
//   - There is exactly one DNS query per verification (the domain's TXT set).
package spf

import (
	"errors"
	"net/netip"
	"strings"
)

// Parse and DNS bounds. Everything untrusted is capped before it is examined.
const (
	MaxRecordBytes = 2048 // published record (TXT strings joined)
	MaxTermBytes   = 255
	MaxTerms       = 64
	MaxTXTRecords  = 64 // TXT records inspected in one answer
)

// InvalidError is a malformed SPF record. Reason is a bounded code, never
// record text, so it is safe in logs, metrics and API responses.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return "spf: invalid record: " + e.Reason }

// Invalid reason codes.
const (
	ReasonTooLong           = "record_too_long"
	ReasonTooManyTerms      = "too_many_terms"
	ReasonTermTooLong       = "term_too_long"
	ReasonUnknownMechanism  = "unknown_mechanism"
	ReasonBadIP             = "bad_ip"
	ReasonBadCIDR           = "bad_cidr"
	ReasonBadArgument       = "bad_argument"
	ReasonDuplicateModifier = "duplicate_modifier"
	ReasonBadModifier       = "bad_modifier"
	ReasonBadEncoding       = "bad_encoding"
	ReasonTooManyTXT        = "too_many_txt_records"
)

// Term is one parsed mechanism or modifier.
type Term struct {
	Qualifier byte         // '+', '-', '~', '?' (mechanisms; default '+')
	Name      string       // lower-case mechanism or modifier name
	Modifier  bool         // true for name=value
	Arg       string       // domain-spec or value, untouched (case preserved)
	Prefix    netip.Prefix // ip4/ip6 only, masked
	Raw       string       // original token, used only to rebuild a merged record
}

// Record is a parsed v=spf1 record.
type Record struct {
	Terms []Term
}

// IsSPF reports whether a TXT value is an SPF record: version tag "v=spf1"
// (case-insensitive) followed by end of text or a space (RFC 7208 4.5).
func IsSPF(txt string) bool {
	if len(txt) < 6 || !strings.EqualFold(txt[:6], "v=spf1") {
		return false
	}
	return len(txt) == 6 || txt[6] == ' '
}

// Parse parses one SPF record. Work is linear in a length that is capped first;
// there is no regular expression and no recursion.
func Parse(txt string) (Record, error) {
	if len(txt) > MaxRecordBytes {
		return Record{}, &InvalidError{ReasonTooLong}
	}
	for i := 0; i < len(txt); i++ {
		if txt[i] < 0x20 || txt[i] >= 0x7f { // SP only: RFC 7208 separates terms with spaces
			return Record{}, &InvalidError{ReasonBadEncoding}
		}
	}
	if !IsSPF(txt) {
		return Record{}, &InvalidError{ReasonBadEncoding}
	}
	tokens := strings.Fields(txt[6:])
	if len(tokens) > MaxTerms {
		return Record{}, &InvalidError{ReasonTooManyTerms}
	}
	var rec Record
	seen := map[string]bool{}
	for _, tok := range tokens {
		if len(tok) > MaxTermBytes {
			return Record{}, &InvalidError{ReasonTermTooLong}
		}
		term, err := parseTerm(tok)
		if err != nil {
			return Record{}, err
		}
		if term.Modifier && (term.Name == "redirect" || term.Name == "exp") {
			if seen[term.Name] {
				return Record{}, &InvalidError{ReasonDuplicateModifier}
			}
			seen[term.Name] = true
		}
		rec.Terms = append(rec.Terms, term)
	}
	return rec, nil
}

func parseTerm(tok string) (Term, error) {
	t := Term{Raw: tok, Qualifier: '+'}
	body := tok
	// A modifier's name is followed by '=' before any ':' or '/'.
	if i := strings.IndexAny(body, ":/="); i > 0 && body[i] == '=' {
		name := strings.ToLower(body[:i])
		if !validModifierName(name) {
			return Term{}, &InvalidError{ReasonBadModifier}
		}
		t.Modifier, t.Name, t.Arg = true, name, body[i+1:]
		if (name == "redirect" || name == "exp") && !plausibleDomainSpec(t.Arg) {
			return Term{}, &InvalidError{ReasonBadArgument}
		}
		return t, nil // unknown modifiers are ignored (RFC 7208 6)
	}
	if strings.IndexByte("+-~?", body[0]) >= 0 {
		t.Qualifier, body = body[0], body[1:]
	}
	name, arg, hasArg := body, "", false
	if i := strings.IndexAny(body, ":/"); i >= 0 {
		name, arg, hasArg = body[:i], body[i:], true
		if body[i] == ':' {
			arg = body[i+1:]
		}
	}
	t.Name = strings.ToLower(name)
	switch t.Name {
	case "all":
		if hasArg {
			return Term{}, &InvalidError{ReasonBadArgument}
		}
	case "include", "exists":
		if !hasArg || body[len(name)] != ':' || !plausibleDomainSpec(arg) {
			return Term{}, &InvalidError{ReasonBadArgument}
		}
		t.Arg = arg
	case "a", "mx":
		if err := checkDualCIDR(&t, arg, hasArg); err != nil {
			return Term{}, err
		}
	case "ptr":
		if hasArg && (body[len(name)] != ':' || !plausibleDomainSpec(arg)) {
			return Term{}, &InvalidError{ReasonBadArgument}
		}
		t.Arg = arg
	case "ip4", "ip6":
		if !hasArg || body[len(name)] != ':' {
			return Term{}, &InvalidError{ReasonBadIP}
		}
		p, err := parseIPArg(t.Name == "ip4", arg)
		if err != nil {
			return Term{}, err
		}
		t.Prefix, t.Arg = p, arg
	default:
		return Term{}, &InvalidError{ReasonUnknownMechanism}
	}
	return t, nil
}

func validModifierName(n string) bool {
	if n == "" || n[0] < 'a' || n[0] > 'z' {
		return false
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// plausibleDomainSpec is a cheap sanity bound, not a full macro grammar: the
// value is never expanded or queried by MailX.
func plausibleDomainSpec(s string) bool { return s != "" && len(s) <= 253 }

// checkDualCIDR validates the optional ":domain" and "/n" or "//n" of a/mx.
func checkDualCIDR(t *Term, arg string, hasArg bool) error {
	if !hasArg {
		return nil
	}
	rest := arg
	if arg != "" && arg[0] != '/' { // ":domain..." was already stripped of ':'
		i := strings.IndexByte(arg, '/')
		if i < 0 {
			i = len(arg)
		}
		if !plausibleDomainSpec(arg[:i]) {
			return &InvalidError{ReasonBadArgument}
		}
		t.Arg, rest = arg[:i], arg[i:]
	}
	if rest == "" {
		return nil
	}
	v4, v6, _ := strings.Cut(rest, "//")
	if v4 != "" {
		if v4[0] != '/' || !cidrOK(v4[1:], 32) {
			return &InvalidError{ReasonBadCIDR}
		}
	}
	if strings.Contains(rest, "//") && !cidrOK(v6, 128) {
		return &InvalidError{ReasonBadCIDR}
	}
	return nil
}

func cidrOK(s string, max int) bool {
	if s == "" || len(s) > 3 {
		return false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n <= max
}

func parseIPArg(v4 bool, arg string) (netip.Prefix, error) {
	addrPart, cidr, hasCIDR := strings.Cut(arg, "/")
	addr, err := netip.ParseAddr(addrPart)
	if err != nil || addr.Zone() != "" || addr.Is4() != v4 {
		return netip.Prefix{}, &InvalidError{ReasonBadIP}
	}
	bits := addr.BitLen()
	if hasCIDR {
		max := 32
		if !v4 {
			max = 128
		}
		if !cidrOK(cidr, max) {
			return netip.Prefix{}, &InvalidError{ReasonBadCIDR}
		}
		bits = 0
		for i := 0; i < len(cidr); i++ {
			bits = bits*10 + int(cidr[i]-'0')
		}
	}
	p, err := addr.Prefix(bits)
	if err != nil {
		return netip.Prefix{}, &InvalidError{ReasonBadCIDR}
	}
	return p, nil
}

// Coverage classifies how a record treats one sending address, using only the
// literal ip4/ip6 mechanisms in order, stopping at the first "all".
type Coverage uint8

const (
	NotListed  Coverage = iota // no literal mechanism matches
	Authorized                 // first literal match has qualifier "+"
	Denied                     // first literal match has -, ~ or ?
)

// Covers reports how the record's literal mechanisms treat addr. Unevaluated
// terms are skipped (see package comment).
func (r Record) Covers(addr netip.Addr) Coverage {
	addr = addr.Unmap()
	for _, t := range r.Terms {
		if t.Modifier {
			continue
		}
		if t.Name == "all" {
			return NotListed
		}
		if (t.Name == "ip4" || t.Name == "ip6") && t.Prefix.Contains(addr) {
			if t.Qualifier == '+' {
				return Authorized
			}
			return Denied
		}
	}
	return NotListed
}

// Unevaluated reports whether the record has terms MailX does not resolve and
// that could authorize (or deny) the address: include, a, mx, exists, ptr
// before any "all", or a redirect modifier when there is no "all".
func (r Record) Unevaluated() bool {
	hasAll := false
	for _, t := range r.Terms {
		if !t.Modifier && t.Name == "all" {
			hasAll = true
			break
		}
		switch t.Name {
		case "include", "a", "mx", "exists", "ptr":
			return true
		}
	}
	if !hasAll {
		for _, t := range r.Terms {
			if t.Modifier && t.Name == "redirect" {
				return true
			}
		}
	}
	return false
}

// DNSTerms counts terms that cost a DNS query during evaluation (RFC 7208
// section 4.6.4 limit: 10, counting nested includes MailX does not follow).
func (r Record) DNSTerms() int {
	n := 0
	for _, t := range r.Terms {
		switch t.Name {
		case "include", "a", "mx", "exists", "ptr", "redirect":
			n++
		}
	}
	return n
}

// Includes reports whether the record has an include:<domain> term (compared
// case-insensitively, trailing dot ignored) that is reachable before any "all".
func (r Record) Includes(domain string) bool {
	want := strings.TrimSuffix(strings.ToLower(domain), ".")
	for _, t := range r.Terms {
		if !t.Modifier && t.Name == "all" {
			return false
		}
		if !t.Modifier && t.Name == "include" && t.Qualifier == '+' &&
			strings.TrimSuffix(strings.ToLower(t.Arg), ".") == want {
			return true
		}
	}
	return false
}

// PermitsAll reports a "+all" (or bare "all") mechanism, which authorizes every
// sender on the Internet. MailX never counts that as verification.
func (r Record) PermitsAll() bool {
	for _, t := range r.Terms {
		if !t.Modifier && t.Name == "all" {
			return t.Qualifier == '+'
		}
	}
	return false
}

var errTooLong = errors.New("spf: merged record exceeds the size bound")

// Merge returns a single record that adds add (mechanism tokens such as
// "ip4:203.0.113.10") to the FRONT of the existing terms. Front insertion
// guarantees the new mechanisms match before any restrictive term, so the
// result is correct for any existing record, and keeps it ONE record.
func (r Record) Merge(add []string) (string, error) {
	parts := append([]string{"v=spf1"}, add...)
	for _, t := range r.Terms {
		parts = append(parts, t.Raw)
	}
	out := strings.Join(parts, " ")
	if len(out) > MaxRecordBytes {
		return "", errTooLong
	}
	return out, nil
}
