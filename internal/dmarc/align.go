package dmarc

import (
	"strings"

	"golang.org/x/net/publicsuffix"

	"github.com/Ferousco-dev/mailx/internal/domain"
)

// OrganizationalDomain returns the registrable domain of d using the Public
// Suffix List (ICANN section), through the same canonicalization MailX uses for
// domain ownership, so the two can never disagree about what a domain is. It is
// deliberately NOT a suffix test: "co.uk" and "com" have no organizational
// domain, and "attackerexample.com" is unrelated to "example.com".
//
// RFC 9989 replaced the PSL with a DNS Tree Walk for the organizational domain;
// the two agree except where a public-suffix operator publishes psd= records or
// the PSL and DNS disagree. MailX uses the PSL for alignment (offline,
// deterministic, no attacker-influenced DNS in the decision) and documents the
// difference as a limitation.
func OrganizationalDomain(d string) (string, error) {
	name, err := domain.Normalize(d)
	if err != nil {
		return "", err
	}
	return publicsuffix.EffectiveTLDPlusOne(name)
}

// Aligned reports DMARC identifier alignment (RFC 9989) between the RFC 5322
// From domain and an authenticated identifier: identical domains in strict mode,
// the same organizational domain in relaxed mode. Comparison is case-insensitive;
// anything that does not canonicalize (IDN, IP literals, public suffixes) is
// never aligned. This is alignment of two names only: it says nothing about
// whether SPF or DKIM passed.
func Aligned(fromDomain, authenticated string, mode Mode) bool {
	from, err1 := domain.Normalize(fromDomain)
	id, err2 := domain.Normalize(authenticated)
	if err1 != nil || err2 != nil {
		return false
	}
	if mode == Strict {
		return from == id
	}
	ofrom, err1 := OrganizationalDomain(from)
	oid, err2 := OrganizationalDomain(id)
	return err1 == nil && err2 == nil && strings.EqualFold(ofrom, oid)
}

// walkNames lists the names whose _dmarc. record can govern domain, from the
// domain itself up to its organizational domain, capped at maxQueries.
func walkNames(d string, maxQueries int) []string {
	name, err := domain.Normalize(d)
	if err != nil {
		return nil
	}
	org, err := OrganizationalDomain(name)
	if err != nil {
		return []string{name}
	}
	names := []string{name}
	for name != org && len(names) < maxQueries {
		_, name, _ = strings.Cut(name, ".")
		names = append(names, name)
	}
	return names
}
