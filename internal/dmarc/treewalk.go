package dmarc

// RFC 9989 section 4.10 DNS Tree Walk, section 4.10.1 policy discovery and
// section 4.10.2 Organizational Domain selection, as ONE operation.
//
// A single walk from a domain yields both the DMARC policy record that governs
// it and its Organizational Domain, so the policy MailX reports and the boundary
// it aligns against can never come from two different algorithms. The Public
// Suffix List plays no part in either.
//
// Rules implemented (section numbers are RFC 9989):
//   4.10 step 1-2: query _dmarc.<start>; discard records whose first tag is not
//        v=DMARC1; if MULTIPLE records remain they are ALL discarded (and the walk
//        continues); stop when a single record carries psd=n or psd=y.
//   4.10 steps 3-7: x = label count; if x < 8 remove the left-most label, else
//        shorten to 7 labels; then remove one label at a time, querying each name
//        down to the single-label TLD. At most 8 queries in total.
//   4.10.2: Organizational Domain = (1) the name of a record with psd=n; else
//        (2) a psd=y record other than the starting domain's own => the domain one
//        label below it; else (3) the name with the fewest labels that has a
//        record; else the starting domain.
//   4.10.1: the policy record is the one at the Author Domain, else at its
//        Organizational Domain, else at its PSD (the psd=y record). For the latter
//        two, sp (the domain exists) applies before p.
//
// Error handling (left to the receiver by the RFC) is fail-safe here: any DNS
// error other than "no such record" ends the walk with an error and nothing is
// concluded: no parent policy is substituted and no boundary is guessed.

import (
	"context"
	"errors"
	"net"
	"strings"
)

// maxQueries is the RFC 9989 section 4.10 cap on queries per walk.
const maxQueries = 8

var errWalkDNS = errors.New("dmarc: DNS error during tree walk")

type entryKind uint8

const (
	kindNone     entryKind = iota // no DMARC record at this name
	kindMultiple                  // several records: all discarded (RFC 9989 4.10 step 2)
	kindRecord                    // exactly one DMARC record (possibly with invalid tags)
)

// entry is what one _dmarc.<name> query returned.
type entry struct {
	name    string
	kind    entryKind
	raw     string
	rec     Record // valid when invalid == ""
	invalid string // parse failure reason when the single record is malformed
	psd     string // y, n or u; read leniently so a malformed record still counts
}

// Policy sources.
const (
	SourceDomain = "domain"
	SourceOrg    = "organizational_domain"
	SourcePSD    = "public_suffix_domain"
)

// policyRecord is the record that governs the starting domain.
type policyRecord struct {
	name    string
	source  string
	raw     string
	rec     Record
	invalid string
}

// walkResult is the single outcome of one tree walk.
type walkResult struct {
	start     string
	org       string
	queries   int
	policy    *policyRecord
	conflicts []string // names whose records were discarded as multiple
}

// targets returns the names to query, in order, for a canonical domain
// (RFC 9989 4.10 steps 1, 3, 4 and 7). At most maxQueries names.
func targets(domain string) []string {
	labels := strings.Split(domain, ".")
	x := len(labels)
	out := []string{domain}
	if x == 1 {
		return out
	}
	n := x - 1
	if x >= 8 {
		n = 7
	}
	for ; n >= 1; n-- {
		out = append(out, strings.Join(labels[x-n:], "."))
	}
	return out
}

func labelCount(name string) int { return strings.Count(name, ".") + 1 }

// classify turns one TXT answer into an entry.
func classify(name string, records []string) entry {
	e := entry{name: name, psd: "u"}
	if len(records) > MaxTXTRecords {
		e.kind, e.invalid = kindRecord, ReasonTooManyTXT
		return e
	}
	var cands []string
	for _, r := range records {
		if IsDMARC(r) {
			cands = append(cands, r)
		}
	}
	switch len(cands) {
	case 0:
		return e
	case 1:
	default:
		e.kind = kindMultiple
		return e
	}
	e.kind, e.raw = kindRecord, cands[0]
	rec, err := Parse(cands[0])
	if err != nil {
		e.invalid = ReasonBadTag
		var ie *InvalidError
		if errors.As(err, &ie) {
			e.invalid = ie.Reason
		}
		e.psd = scanPSD(cands[0])
		return e
	}
	e.rec, e.psd = rec, rec.PSD
	return e
}

// scanPSD reads only the psd tag of a record that failed full parsing, so a
// malformed record still stops the walk and selects the Organizational Domain
// exactly as the RFC says (it is a DMARC Policy Record retrieved for the name).
func scanPSD(txt string) string {
	if len(txt) > MaxRecordBytes {
		return "u"
	}
	for _, part := range strings.Split(txt, ";") {
		name, val, ok := strings.Cut(part, "=")
		if ok && strings.EqualFold(trimWSP(name), "psd") {
			switch v := strings.ToLower(trimWSP(val)); v {
			case "y", "n":
				return v
			}
			return "u"
		}
	}
	return "u"
}

// treeWalk performs the bounded walk for a canonical domain.
func (s *Service) treeWalk(ctx context.Context, start string) (*walkResult, error) {
	var entries []entry
	for _, name := range targets(start) {
		records, err := s.resolver.LookupTXT(ctx, dmarcLabel+name)
		if err != nil {
			var dnsErr *net.DNSError
			if errors.As(err, &dnsErr) && dnsErr.IsNotFound && !dnsErr.IsTemporary && !dnsErr.IsTimeout {
				entries = append(entries, entry{name: name, kind: kindNone, psd: "u"})
				continue
			}
			return nil, errWalkDNS
		}
		e := classify(name, records)
		entries = append(entries, e)
		if e.kind == kindRecord && (e.psd == "y" || e.psd == "n") {
			break // RFC 9989 4.10 steps 2 and 6
		}
	}
	return resolve(start, entries), nil
}

// resolve is the pure core: entries (longest name first, as queried) ->
// Organizational Domain (4.10.2) and governing policy record (4.10.1).
func resolve(start string, entries []entry) *walkResult {
	w := &walkResult{start: start, org: start, queries: len(entries)}
	orgSet := false
	var last *entry
	for i := range entries {
		e := &entries[i]
		if e.kind == kindMultiple {
			w.conflicts = append(w.conflicts, e.name)
			continue
		}
		if e.kind != kindRecord {
			continue
		}
		last = e
		if orgSet {
			continue
		}
		switch {
		case e.psd == "n":
			w.org, orgSet = e.name, true
		case e.psd == "y" && e.name != start:
			w.org, orgSet = oneLabelBelow(start, e.name), true
		}
	}
	if !orgSet && last != nil {
		w.org = last.name // fewest labels among names that have a record
	}
	w.policy = pickPolicy(start, w.org, entries)
	return w
}

// oneLabelBelow returns the domain one label below ancestor on the path to start.
func oneLabelBelow(start, ancestor string) string {
	labels := strings.Split(start, ".")
	k := labelCount(ancestor) + 1
	if k > len(labels) {
		return start
	}
	return strings.Join(labels[len(labels)-k:], ".")
}

// pickPolicy applies RFC 9989 4.10.1 preference: Author Domain, then its
// Organizational Domain, then its PSD.
func pickPolicy(start, org string, entries []entry) *policyRecord {
	find := func(name string) *entry {
		for i := range entries {
			if entries[i].name == name && entries[i].kind == kindRecord {
				return &entries[i]
			}
		}
		return nil
	}
	mk := func(e *entry, source string) *policyRecord {
		return &policyRecord{name: e.name, source: source, raw: e.raw, rec: e.rec, invalid: e.invalid}
	}
	if e := find(start); e != nil {
		return mk(e, SourceDomain)
	}
	if org != start {
		if e := find(org); e != nil {
			return mk(e, SourceOrg)
		}
	}
	for i := range entries {
		if e := &entries[i]; e.kind == kindRecord && e.psd == "y" && e.name != start {
			return mk(e, SourcePSD)
		}
	}
	return nil
}
