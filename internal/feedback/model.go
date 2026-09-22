// Package feedback processes ASYNCHRONOUS feedback about mail MailX previously
// sent: delivery-status notifications (RFC 3464 DSNs/bounces) and complaint
// reports. It is pure (no I/O, no database): parsing, classification and
// correlation only, mirroring internal/suppression's split from
// internal/database/suppressions.go (persistence lives in
// internal/database/feedback.go; the trust/authentication boundary lives in
// internal/api).
//
// This is NOT general inbound email. MailX does not host mailboxes, does not
// route arbitrary inbound mail, and this package accepts exactly one thing:
// feedback about a message MailX itself sent, correlated back to that message
// and never trusted to mutate another tenant's data (see Correlator).
package feedback

import "time"

// Kind is a classified feedback outcome. Only bounce_permanent and complaint
// are ever suppression-worthy (see suppression.QualifiesAsyncHardBounce); the
// others are recorded as history and nothing else — see docs/design-v0.32.md
// "Bounce classification" for the conservative rule this encodes.
type Kind string

const (
	KindBouncePermanent Kind = "bounce_permanent" // Action: failed, 5.x.x
	KindBounceTemporary Kind = "bounce_temporary" // Action: delayed, 4.x.x — NEVER suppresses
	KindBounceUnknown   Kind = "bounce_unknown"   // malformed/missing/unrecognized DSN fields — NEVER suppresses
	KindComplaint       Kind = "complaint"        // a verified complaint report — suppresses
)

// Source names where a Kind came from.
type Source string

const (
	SourceDSN       Source = "dsn"
	SourceComplaint Source = "complaint"
)

// Action is RFC 3464's per-recipient Action field (delivery-status "Action:"),
// restricted to values MailX classifies on. "relayed" and "expanded" are
// recognized so a legitimate DSN using them is not misparsed as malformed, but
// they carry no MailX meaning (never suppress, never treated as failure).
type Action string

const (
	ActionFailed    Action = "failed"
	ActionDelayed   Action = "delayed"
	ActionDelivered Action = "delivered"
	ActionRelayed   Action = "relayed"
	ActionExpanded  Action = "expanded"
	ActionUnknown   Action = "unknown"
)

func parseAction(s string) Action {
	switch Action(s) {
	case ActionFailed, ActionDelayed, ActionDelivered, ActionRelayed, ActionExpanded:
		return Action(s)
	default:
		return ActionUnknown
	}
}

// Bounded input limits. Feedback is untrusted Internet/provider-adjacent input
// (see the package doc); every limit here exists so malformed or hostile input
// costs O(1) memory and cannot make the parser allocate, recurse, or scan
// without bound (RFC 3462/3464 place no upper bound on any of this, so MailX
// must).
const (
	// MaxInputBytes bounds a raw feedback submission (multipart/report DSN or a
	// JSON complaint body) before ANY parsing begins.
	MaxInputBytes = 256 << 10 // 256 KiB: generous for a delivery-status report plus a
	// truncated copy of the original headers, far below what could pressure memory.
	maxMIMEParts     = 16 // multipart/report has exactly 2-3 parts in practice; RFC 3462 does not bound this.
	maxHeaderLine    = 4 << 10
	maxDiagnosticLen = 512
	maxMTALen        = 255
)

// ParsedDSN is the trustworthy STRUCTURED subset of one RFC 3464
// message/delivery-status field group. Per-message (not per-recipient) fields
// (ReportingMTA) are parsed once; per-recipient fields describe exactly one
// original recipient, matching RFC 3464 3.2's "one or more" delivery-status
// per-recipient field groups — ParseDSN returns one ParsedDSN per group.
//
// Deliberately excluded: arbitrary free-text diagnostic beyond MaxDiagnosticLen,
// the human-readable part (never used for classification — see
// docs/design-v0.32.md "Bounce classification": text like "mailbox doesn't
// exist" is never trusted), and the original message body.
type ParsedDSN struct {
	Action            Action
	Status            string // raw enhanced-status token, validated by bounce.ParseEnhancedStatus by the caller
	FinalRecipient    string // address only, "rfc822;" type prefix stripped
	OriginalRecipient string // address only, empty if absent
	DiagnosticCode    string // truncated to maxDiagnosticLen, control characters stripped
	RemoteMTA         string // truncated to maxMTALen
	ReportingMTA      string // truncated to maxMTALen
}

// Feedback is the normalized item ready for correlation and persistence: what
// database.ProcessFeedback needs, independent of whether it came from a DSN or
// a complaint report.
type Feedback struct {
	Kind              Kind
	Source            Source
	CorrelationToken  string // as submitted; verified by Correlator.Verify before use
	Action            Action
	EnhancedStatus    string // canonical "class.subject.detail" or empty
	Diagnostic        string
	RemoteMTA         string
	ReportingMTA      string
	FinalRecipient    string // address only
	OriginalRecipient string
	ReceivedAt        time.Time
	RawSHA256         [32]byte
}
