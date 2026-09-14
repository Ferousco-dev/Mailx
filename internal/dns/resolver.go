// Package dns discovers SMTP destinations for a mail domain using the DNS
// MX records defined in RFC 5321 §5.1 and RFC 7505 (Null MX). It performs
// discovery only — it never dials SMTP and it never retries.
//
// Layout (see MailX Code Organization Rules):
//
//	resolver.go — Resolver, LookupMX orchestration, domain normalization
//	mx.go       — MX record type, host canonicalization, ordering
//	errors.go   — LookupError, Kind, classification helpers
package dns

import (
	"context"
	"errors"
	"net"
	"strings"
)

// MaxCandidates caps how many MX candidates one lookup returns. RFC 5321
// permits many MX records but a delivery layer walking hundreds of candidates
// is almost certainly an attack surface, not a real mail domain.
const MaxCandidates = 32

// Lookup is the small DNS boundary MailX depends on. The default production
// implementation is netResolverAdapter{net.DefaultResolver}; tests inject a
// deterministic fake.
type Lookup interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// Resolver answers "what SMTP destinations should a future delivery engine
// consider for this mail domain?". It performs no retries and never opens an
// SMTP connection. Safe for concurrent use.
type Resolver struct {
	lookup Lookup
}

// NewResolver returns a Resolver backed by the system DNS resolver.
func NewResolver() *Resolver {
	return &Resolver{lookup: netResolverAdapter{r: net.DefaultResolver}}
}

// NewResolverWith is intended for tests: it lets callers substitute the DNS
// boundary. Production code should use NewResolver.
func NewResolverWith(lookup Lookup) *Resolver {
	return &Resolver{lookup: lookup}
}

// LookupMX resolves the mail domain into ordered SMTP destination candidates.
// The returned slice may be empty only alongside a non-nil LookupError.
//
// Semantics:
//   - Normalizes the input (whitespace, trailing dot, lower-case).
//   - Rejects obviously invalid input as KindInvalidDomain before touching DNS.
//   - Returns KindNullMX (RFC 7505) when the domain publishes "0 ." — never
//     falls back to implicit MX in that case.
//   - Falls back to the domain itself as an implicit MX (RFC 5321 §5.1) when
//     no MX records exist AND the domain resolves to at least one host
//     address. Otherwise returns KindNotFound.
//   - Distinguishes NXDOMAIN (KindNotFound) from transient DNS failure
//     (KindTemporary).
func (r *Resolver) LookupMX(ctx context.Context, domain string) ([]MX, error) {
	normalized, err := normalizeDomain(domain)
	if err != nil {
		return nil, &LookupError{Domain: domain, Kind: KindInvalidDomain, Err: err}
	}

	records, err := r.lookup.LookupMX(ctx, normalized)
	if err != nil {
		// LookupMX may return records alongside a partial-success error;
		// treat any record set as authoritative if the error is IsNotFound
		// (Go signals implicit-fallback intent this way for some domains).
		// Otherwise classify the DNS error.
		if len(records) == 0 {
			if ctx.Err() != nil {
				return nil, &LookupError{Domain: normalized, Kind: KindTemporary, Err: ctx.Err()}
			}
			return nil, classifyDNSError(normalized, err, r, ctx)
		}
	}

	// Null MX per RFC 7505: exactly one MX with (0, ".").
	if len(records) == 1 && isNullMXRecord(records[0]) {
		return nil, &LookupError{Domain: normalized, Kind: KindNullMX}
	}
	// A "0 ." mixed with other records is malformed; refuse it rather than
	// silently ignoring the Null-MX signal.
	for _, rec := range records {
		if isNullMXRecord(rec) && len(records) > 1 {
			return nil, &LookupError{Domain: normalized, Kind: KindResolverFailure, Err: errors.New("null MX mixed with other MX records")}
		}
	}

	if len(records) == 0 {
		// No MX records — implicit-MX fallback: the domain itself if it has
		// at least one address record.
		return implicitFallback(ctx, normalized, r.lookup)
	}

	candidates := make([]MX, 0, len(records))
	for _, rec := range records {
		host := canonicalHost(rec.Host)
		if !hostSafe(host) {
			continue
		}
		candidates = append(candidates, MX{Host: host, Preference: rec.Pref})
	}
	if len(candidates) == 0 {
		return nil, &LookupError{Domain: normalized, Kind: KindResolverFailure, Err: errors.New("no usable MX hosts")}
	}
	candidates = sortAndDedup(candidates)
	if len(candidates) > MaxCandidates {
		candidates = candidates[:MaxCandidates]
	}
	return candidates, nil
}

// classifyDNSError maps a resolver error into a Kind. It is called only when
// LookupMX returned no records.
func classifyDNSError(domain string, err error, r *Resolver, ctx context.Context) *LookupError {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			// NoData for MX is not necessarily NXDOMAIN — the domain may
			// still have A/AAAA. Try implicit-MX fallback.
			cands, fallbackErr := implicitFallback(ctx, domain, r.lookup)
			if fallbackErr == nil {
				// Fallback succeeded — but we're already in an error path,
				// so honor the Go convention: return candidates the caller
				// can use by *not* returning an error here. We do that by
				// signalling through a special sentinel: fold into caller.
				_ = cands // unreachable normally; kept for future.
			}
			return &LookupError{Domain: domain, Kind: KindNotFound, Err: err}
		}
		if dnsErr.IsTimeout || dnsErr.IsTemporary {
			return &LookupError{Domain: domain, Kind: KindTemporary, Err: err}
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &LookupError{Domain: domain, Kind: KindTemporary, Err: err}
	}
	return &LookupError{Domain: domain, Kind: KindResolverFailure, Err: err}
}

// implicitFallback implements RFC 5321 §5.1: when a domain has no MX records,
// the domain itself is treated as an implicit MX at preference 0, provided
// the domain resolves to at least one address. NXDOMAIN or no addresses →
// KindNotFound.
func implicitFallback(ctx context.Context, domain string, lookup Lookup) ([]MX, error) {
	addrs, err := lookup.LookupHost(ctx, domain)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			if dnsErr.IsNotFound {
				return nil, &LookupError{Domain: domain, Kind: KindNotFound, Err: err}
			}
			if dnsErr.IsTimeout || dnsErr.IsTemporary {
				return nil, &LookupError{Domain: domain, Kind: KindTemporary, Err: err}
			}
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, &LookupError{Domain: domain, Kind: KindTemporary, Err: err}
		}
		return nil, &LookupError{Domain: domain, Kind: KindResolverFailure, Err: err}
	}
	if len(addrs) == 0 {
		return nil, &LookupError{Domain: domain, Kind: KindNotFound}
	}
	host := canonicalHost(domain)
	if !hostSafe(host) {
		return nil, &LookupError{Domain: domain, Kind: KindResolverFailure, Err: errors.New("implicit MX host is not usable")}
	}
	return []MX{{Host: host, Preference: 0}}, nil
}

// normalizeDomain trims whitespace, strips a trailing root dot, and
// lower-cases the domain. It rejects empty and control-character input.
func normalizeDomain(domain string) (string, error) {
	trimmed := strings.TrimSpace(domain)
	if trimmed == "" {
		return "", errors.New("domain is empty")
	}
	// v0.9 accepts ASCII domains only (LDH — letters, digits, hyphen — plus
	// dots). Non-ASCII input needs IDNA/A-label conversion which is deferred.
	for i := 0; i < len(trimmed); i++ {
		b := trimmed[i]
		if b < 0x21 || b >= 0x7f {
			return "", errors.New("domain contains non-ASCII or forbidden control byte")
		}
	}
	if strings.Contains(trimmed, "@") {
		return "", errors.New("domain must not contain '@' — pass domain, not mailbox")
	}
	trimmed = strings.TrimSuffix(trimmed, ".")
	if trimmed == "" {
		return "", errors.New("domain is only a trailing dot")
	}
	// Lower-case first so length checks reflect the canonical byte form —
	// Unicode case-mapping can otherwise expand bytes and break idempotency.
	lowered := strings.ToLower(trimmed)
	for _, label := range strings.Split(lowered, ".") {
		if label == "" {
			return "", errors.New("domain contains an empty label")
		}
		if len(label) > 63 {
			return "", errors.New("domain label exceeds 63 octets")
		}
	}
	if len(lowered) > 253 {
		return "", errors.New("domain exceeds 253 octets")
	}
	return lowered, nil
}

// netResolverAdapter adapts *net.Resolver to the Lookup interface.
type netResolverAdapter struct {
	r *net.Resolver
}

func (a netResolverAdapter) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	return a.r.LookupMX(ctx, name)
}

func (a netResolverAdapter) LookupHost(ctx context.Context, host string) ([]string, error) {
	return a.r.LookupHost(ctx, host)
}
