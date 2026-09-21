package dmarc

import (
	"net"
	"strings"
)

// canonical lowercases and validates a DNS name for comparison: ASCII LDH labels
// only (MailX supports no IDN/A-labels anywhere), no IP literals, no empty or
// over-long labels. It is a syntax gate, not a boundary: it says nothing about
// where an organization begins. That is decided only by the RFC 9989 DNS Tree
// Walk (treewalk.go).
func canonical(d string) (string, bool) {
	d = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
	if d == "" || len(d) > 253 || net.ParseIP(d) != nil {
		return "", false
	}
	for _, label := range strings.Split(d, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' || strings.HasPrefix(label, "xn--") {
			return "", false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", false
			}
		}
	}
	return d, true
}

// OrgFunc returns the Organizational Domain of a domain as determined by an
// RFC 9989 DNS Tree Walk. ok is false when that could not be determined (DNS
// failure), which is different from "not aligned".
type OrgFunc func(domain string) (org string, ok bool)

// Aligned reports RFC 9989 identifier alignment (section 4.4 and 4.10.2)
// between the RFC 5322 From domain and an authenticated identifier.
//
//   - Identical domains are aligned in both modes with no DNS at all.
//   - Strict mode is a plain string comparison of the canonical names.
//   - Relaxed mode compares Organizational Domains obtained from org, which must
//     be backed by the DNS Tree Walk. There is no suffix test and no label-count
//     guess.
//
// known is false only when relaxed alignment needed an Organizational Domain
// that could not be determined; aligned is then false and must not be read as
// "not aligned". Alignment relates two names; it does not say SPF or DKIM passed.
func Aligned(fromDomain, authenticated string, mode Mode, org OrgFunc) (aligned, known bool) {
	from, ok1 := canonical(fromDomain)
	id, ok2 := canonical(authenticated)
	if !ok1 || !ok2 {
		return false, true
	}
	if from == id {
		return true, true
	}
	if mode == Strict {
		return false, true
	}
	if org == nil {
		return false, false
	}
	of, ok1 := org(from)
	oi, ok2 := org(id)
	if !ok1 || !ok2 {
		return false, false
	}
	return of == oi, true
}
