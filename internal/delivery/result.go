package delivery

import (
	"errors"
	"fmt"
	"time"

	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/transfer"
)

// Kind classifies a delivery outcome so a future retry engine (v0.11) can
// decide whether to re-schedule. v0.10 itself never retries.
type Kind string

const (
	KindAccepted          Kind = "accepted"
	KindInvalidRequest    Kind = "invalid_request"
	KindDNSNotFound       Kind = "dns_not_found"
	KindDNSNullMX         Kind = "dns_null_mx"
	KindDNSTemporary      Kind = "dns_temporary"
	KindDNSFailure        Kind = "dns_failure"
	KindTransferTemporary Kind = "transfer_temporary"
	KindTransferPermanent Kind = "transfer_permanent"
	KindContext           Kind = "context"
)

// Attempt records one MX try. Transfer holds the underlying transfer.Result
// (its AttemptID, timestamps, SMTP fields). MX is the candidate that was
// tried; TransferErr holds the *transfer.TransferError if the attempt failed.
type Attempt struct {
	MX          dns.MX
	Destination string
	Transfer    transfer.Result
	TransferErr *transfer.TransferError
}

// Result reports what one immediate delivery operation produced. Even when
// Kind is a failure, Attempts is populated with every MX that was contacted
// so v0.11 (retry) and observability get full history.
type Result struct {
	DeliveryID string
	Domain     string
	// Transport is "direct" (MX lookup, no credentials) or "relay" (the one
	// configured trusted relay, TLS then AUTH). It is fixed by configuration and
	// never changes during a delivery: a relay failure never becomes a direct
	// attempt.
	Transport  string
	StartedAt  time.Time
	FinishedAt time.Time
	Kind       Kind
	Accepted   bool
	// FinalCode / EnhancedStatus / RemoteMessage / FailureStage / Recipient
	// mirror the ATTEMPT that determined the outcome (the accepted attempt
	// on success; the highest-precedence failure on error). FinalCode is 0
	// for non-SMTP failures — MailX never fabricates SMTP codes.
	FinalCode      int
	EnhancedStatus string
	RemoteMessage  string
	FailureStage   string
	Recipient      string
	FailureMessage string
	QuitError      string
	Attempts       []Attempt
	MXCandidates   []dns.MX
	// EffectiveHostname / EffectiveSourceIP (v0.39) are the outbound EHLO
	// identity and dial-source IP actually used for this operation, set by
	// a routing.Router when Request.MemberID selected a direct pool
	// member. Both are empty on the legacy default-Engine path — Engine
	// itself never sets them, only a Router wrapping several Engines does.
	// Never a secret. Persisted verbatim as delivery_attempts' historical
	// transport snapshot.
	EffectiveHostname string
	EffectiveSourceIP string
	// EffectiveMemberID mirrors Request.MemberID when a routing.Router
	// actually dispatched to that member (not on the legacy/fallback
	// path), so the persistence layer can snapshot which member id was
	// live for this specific attempt without re-deriving it.
	EffectiveMemberID string
}

// Duration reports how long the delivery operation took.
func (r Result) Duration() time.Duration { return r.FinishedAt.Sub(r.StartedAt) }

// Temporary reports whether v0.11 should consider re-scheduling later.
func (r Result) Temporary() bool {
	switch r.Kind {
	case KindDNSTemporary, KindTransferTemporary, KindContext:
		return true
	}
	return false
}

// Error is returned alongside a non-accepted Result. Result already carries
// full attempt history, so the error stays small: it lets callers use
// errors.Is / errors.As on the underlying cause.
type Error struct {
	Kind      Kind
	Domain    string
	Temporary bool
	Err       error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("delivery %s (%s): %v", e.Kind, e.Domain, e.Err)
	}
	return fmt.Sprintf("delivery %s (%s)", e.Kind, e.Domain)
}

func (e *Error) Unwrap() error { return e.Err }

// IsTemporary reports whether err is a transient delivery failure.
func IsTemporary(err error) bool {
	var d *Error
	if errors.As(err, &d) {
		return d.Temporary
	}
	return false
}
