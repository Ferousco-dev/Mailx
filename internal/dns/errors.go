package dns

import (
	"errors"
	"fmt"
)

// Kind classifies a resolution outcome so higher layers can decide whether to
// retry (Temporary/ResolverFailure), permanently reject (InvalidDomain,
// NotFound, NullMX), or fail cleanly (context errors passed through).
type Kind string

const (
	// KindInvalidDomain — MailX rejected the input before touching DNS.
	KindInvalidDomain Kind = "invalid_domain"
	// KindNotFound — DNS says the domain does not exist (NXDOMAIN or no
	// usable candidate after implicit-MX fallback).
	KindNotFound Kind = "not_found"
	// KindNullMX — RFC 7505: domain has explicitly declared it does not
	// accept email. Never fall back to implicit MX in this case.
	KindNullMX Kind = "null_mx"
	// KindTemporary — transient DNS failure (SERVFAIL, timeout, refused).
	KindTemporary Kind = "temporary"
	// KindResolverFailure — DNS returned a response that MailX cannot use
	// (malformed hostname, etc.). Permanent from MailX's perspective.
	KindResolverFailure Kind = "resolver_failure"
)

// LookupError carries the resolution outcome plus the underlying cause.
type LookupError struct {
	Domain string
	Kind   Kind
	Err    error
}

func (e *LookupError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("dns lookup %s: %s: %v", e.Domain, e.Kind, e.Err)
	}
	return fmt.Sprintf("dns lookup %s: %s", e.Domain, e.Kind)
}

func (e *LookupError) Unwrap() error { return e.Err }

// IsTemporary reports whether err is a transient DNS failure.
func IsTemporary(err error) bool {
	var l *LookupError
	if errors.As(err, &l) {
		return l.Kind == KindTemporary
	}
	return false
}

// IsNullMX reports whether the domain published an RFC 7505 Null MX.
func IsNullMX(err error) bool {
	var l *LookupError
	if errors.As(err, &l) {
		return l.Kind == KindNullMX
	}
	return false
}
