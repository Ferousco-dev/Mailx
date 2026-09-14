package dns

import (
	"net"
	"sort"
	"strings"
)

// MX is one usable SMTP destination candidate returned by the resolver. Host
// is a DNS hostname (no port, trailing root dot stripped, lower-cased);
// callers pair it with a port and dialer in the delivery layer.
type MX struct {
	Host       string
	Preference uint16
}

// canonicalHost strips a trailing root dot and lower-cases the DNS name.
func canonicalHost(host string) string {
	host = strings.TrimSpace(host)
	host = strings.TrimSuffix(host, ".")
	return strings.ToLower(host)
}

// isNullMXRecord reports whether a resolver-returned MX record matches
// RFC 7505's "0 ." signalling that the domain does not accept mail.
func isNullMXRecord(r *net.MX) bool {
	if r == nil {
		return false
	}
	host := strings.TrimSpace(r.Host)
	// Go's resolver typically returns "." for the root exchange.
	return r.Pref == 0 && (host == "." || host == "")
}

// hostSafe rejects hostnames MailX cannot safely hand to future SMTP dialers.
// It is intentionally narrow: obvious injection/empty cases only.
func hostSafe(host string) bool {
	if host == "" {
		return false
	}
	if strings.ContainsAny(host, "\r\n\x00 \t") {
		return false
	}
	return true
}

// sortAndDedup orders MX records by preference (ascending) and removes exact
// (host, preference) duplicates. Records with equal preference keep the order
// the resolver returned them (see resolver.go for the equal-preference note).
func sortAndDedup(records []MX) []MX {
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].Preference < records[j].Preference
	})
	out := records[:0]
	seen := make(map[MX]struct{}, len(records))
	for _, r := range records {
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	return out
}
