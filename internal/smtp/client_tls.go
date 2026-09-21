// Package smtp — outbound STARTTLS (RFC 3207).
//
// The state machine this file owns, in order:
//
//	220 greeting -> EHLO -> capabilities -> policy decision ->
//	STARTTLS -> 220 -> TLS handshake (bounded) -> DISCARD capabilities ->
//	EHLO again -> NEW capabilities -> MAIL FROM ...
//
// Invariants (each covered by a test):
//   - Capabilities learned before the handshake are dropped the moment the
//     handshake is attempted; MAIL/RCPT/DATA only ever run after a fresh EHLO.
//   - A failed STARTTLS or handshake is an error. The client never retries the
//     same peer in plaintext, and never sends MAIL FROM after a failed upgrade.
//   - Under TLSRequired the client sends nothing but EHLO/STARTTLS/QUIT-class
//     traffic to a peer that does not advertise STARTTLS.
//   - Certificates are verified against the system roots (plus any operator
//     supplied roots) and the MX host name. There is no insecure-skip option.
//   - Error text exposed upward is a bounded category, never raw TLS error
//     strings (which can carry peer-controlled certificate names).
package smtp

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"time"
)

// TLSPolicy selects how outbound STARTTLS is used. The zero value is
// TLSOpportunistic, the interoperable default for direct Internet delivery.
type TLSPolicy uint8

const (
	// TLSOpportunistic upgrades to TLS when the peer advertises STARTTLS and
	// continues in plaintext only when it does NOT advertise it (RFC 3207
	// forbids public servers from requiring STARTTLS, so plaintext must stay
	// possible). Once STARTTLS is attempted, any failure is a delivery
	// failure: there is no fall back to plaintext.
	TLSOpportunistic TLSPolicy = iota
	// TLSRequired refuses to send a message unless TLS was negotiated.
	TLSRequired
)

func (p TLSPolicy) String() string {
	if p == TLSRequired {
		return "required"
	}
	return "opportunistic"
}

// ParseTLSPolicy parses "opportunistic" or "required" (case-insensitive).
// The empty string selects the default, opportunistic.
func ParseTLSPolicy(s string) (TLSPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "opportunistic":
		return TLSOpportunistic, nil
	case "required":
		return TLSRequired, nil
	}
	return 0, fmt.Errorf("invalid SMTP TLS policy %q (want opportunistic or required)", s)
}

// TLSConfig is the small set of outbound TLS settings.
type TLSConfig struct {
	Policy TLSPolicy
	// RootCAs, when non-nil, replaces the system trust roots. It exists for
	// private CAs and for tests; verification is never disabled.
	RootCAs *x509.CertPool
	// HandshakeTimeout bounds the TLS handshake. Zero selects a finite default.
	HandshakeTimeout time.Duration
	// Observer receives one bounded event per Send that reached the TLS
	// decision. Nil disables it. A panicking observer cannot affect delivery.
	Observer TLSObserver
}

const defaultTLSHandshakeTimeout = 30 * time.Second

// TLSObserver receives bounded, privacy-safe TLS facts. policy is
// "opportunistic" or "required"; outcome is one of the TLSOutcome values;
// version is "1.2", "1.3", "other" or "none".
type TLSObserver interface {
	TLSResult(policy, outcome, version string)
}

// TLSOutcome is the bounded set of TLS results, safe as a metric label.
type TLSOutcome string

const (
	OutcomeEstablished       TLSOutcome = "established"
	OutcomeNotOffered        TLSOutcome = "not_offered" // opportunistic: plaintext, STARTTLS not advertised
	OutcomeRequiredNoTLS     TLSOutcome = "required_unavailable"
	OutcomeRejected          TLSOutcome = "rejected" // STARTTLS refused or malformed reply
	OutcomeHandshakeTimeout  TLSOutcome = "handshake_timeout"
	OutcomeVerifyFailed      TLSOutcome = "verify_failed"
	OutcomeHandshakeFailed   TLSOutcome = "handshake_failed"
	OutcomeConnectionLost    TLSOutcome = "connection_lost"
	OutcomeEHLOFailed        TLSOutcome = "ehlo_failed" // post-TLS EHLO failed
	OutcomeCanceled          TLSOutcome = "canceled"
	OutcomeProtocolViolation TLSOutcome = "protocol_error"
)

// TLSInfo describes the TLS decision for one Send. It carries no host names,
// addresses or certificate data.
type TLSInfo struct {
	Policy      TLSPolicy
	Advertised  bool
	Attempted   bool
	Established bool
	Version     string
	Outcome     TLSOutcome
}

// TLSFailure is the bounded error placed under a DeliveryError for TLS
// problems. Error() is a fixed category; the raw cause is reachable only
// through Unwrap, so peer-controlled text is never printed or logged.
type TLSFailure struct {
	Outcome TLSOutcome
	cause   error
}

func (e *TLSFailure) Error() string { return "tls " + string(e.Outcome) }
func (e *TLSFailure) Unwrap() error { return e.cause }

// capabilities holds the extension keywords from ONE EHLO reply. A new value
// replaces the old one after every EHLO; nothing accumulates.
type capabilities map[string]string

func (c capabilities) has(keyword string) bool {
	_, ok := c[strings.ToUpper(keyword)]
	return ok
}

// parseCapabilities reads an EHLO reply text (lines joined by "\n"). The first
// line is the server greeting and is not a keyword.
func parseCapabilities(text string) capabilities {
	lines := strings.Split(text, "\n")
	caps := capabilities{}
	for _, line := range lines[min(1, len(lines)):] {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		caps[strings.ToUpper(fields[0])] = strings.Join(fields[1:], " ")
	}
	return caps
}

// secure applies the TLS policy after the first EHLO. On success the session
// is either in plaintext (opportunistic, STARTTLS not offered) or upgraded with
// fresh capabilities from a post-TLS EHLO.
func (s *clientSession) secure(ctx context.Context) *DeliveryError {
	s.tls.Policy = s.config.TLS.Policy
	s.tls.Advertised = s.caps.has("STARTTLS")
	if !s.tls.Advertised {
		if s.config.TLS.Policy == TLSRequired {
			return s.tlsFail(StageStartTLS, OutcomeRequiredNoTLS, nil)
		}
		s.tls.Outcome = OutcomeNotOffered
		return nil
	}
	s.tls.Attempted = true

	code, enh, msg, err := s.command("STARTTLS")
	if err != nil {
		return s.tlsFail(StageStartTLS, classifyIOError(ctx, err), err)
	}
	if code != 220 {
		// A server that advertised STARTTLS and then refuses it is either
		// misconfigured or under attack; neither says anything permanent about
		// the recipient, so the failure is temporary. No plaintext fallback.
		e := replyError(StageStartTLS, code, enh, msg)
		e.Temporary = true
		s.tls.Outcome = OutcomeRejected
		return e
	}
	// Bytes the peer sent after its 220 but before the handshake would be
	// interpreted as plaintext inside the TLS session (command injection).
	if s.reader.Buffered() > 0 {
		return s.tlsFail(StageStartTLS, OutcomeProtocolViolation, errors.New("data after STARTTLS response"))
	}

	// Everything learned in plaintext is untrusted from here on.
	s.caps = nil

	hctx, cancel := context.WithTimeout(ctx, s.config.TLS.HandshakeTimeout)
	tc := tls.Client(s.raw, s.tlsClientConfig())
	err = tc.HandshakeContext(hctx)
	cancel()
	if err != nil {
		return s.tlsFail(StageTLS, classifyHandshakeError(ctx, err), err)
	}
	s.conn = tc
	s.reader = bufio.NewReaderSize(tc, s.config.MaxReplyLineBytes*2)
	s.tls.Established = true
	s.tls.Version = versionLabel(tc.ConnectionState().Version)

	// RFC 3207 section 4.2: EHLO again, and trust only this reply. No HELO
	// fallback: a server that offered STARTTLS supports EHLO.
	code, enh, msg, err = s.command("EHLO " + s.config.Identity)
	if err != nil {
		return s.tlsFail(StageEHLOTLS, classifyIOError(ctx, err), err)
	}
	if code != 250 {
		e := replyError(StageEHLOTLS, code, enh, msg)
		e.Temporary = true
		s.tls.Outcome = OutcomeEHLOFailed
		return e
	}
	s.caps = parseCapabilities(msg)
	s.tls.Outcome = OutcomeEstablished
	return nil
}

func (s *clientSession) tlsClientConfig() *tls.Config {
	return &tls.Config{
		ServerName: s.serverName,
		RootCAs:    s.config.TLS.RootCAs,
		// TLS 1.0 and 1.1 are deprecated (RFC 8996). This equals Go's own
		// default; it is stated so the floor cannot silently drop.
		MinVersion: tls.VersionTLS12,
	}
}

// tlsFail records the outcome and builds a temporary, bounded DeliveryError.
// Temporary because every TLS failure is a statement about this connection,
// not about the recipient; the delivery engine may try another MX, and the
// retry engine retries later.
func (s *clientSession) tlsFail(stage Stage, outcome TLSOutcome, cause error) *DeliveryError {
	s.tls.Outcome = outcome
	return &DeliveryError{Stage: stage, Temporary: true, Err: &TLSFailure{Outcome: outcome, cause: cause}}
}

func classifyIOError(ctx context.Context, err error) TLSOutcome {
	switch {
	case ctx.Err() != nil:
		return OutcomeCanceled
	case isTimeout(err):
		return OutcomeHandshakeTimeout
	case isConnLost(err):
		return OutcomeConnectionLost
	default:
		return OutcomeProtocolViolation
	}
}

func classifyHandshakeError(ctx context.Context, err error) TLSOutcome {
	var (
		cve *tls.CertificateVerificationError
		ua  x509.UnknownAuthorityError
		he  x509.HostnameError
		ci  x509.CertificateInvalidError
	)
	switch {
	case ctx.Err() != nil:
		return OutcomeCanceled
	case errors.As(err, &cve), errors.As(err, &ua), errors.As(err, &he), errors.As(err, &ci):
		return OutcomeVerifyFailed
	case isTimeout(err):
		return OutcomeHandshakeTimeout
	case isConnLost(err):
		return OutcomeConnectionLost
	default:
		return OutcomeHandshakeFailed
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

func isConnLost(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

func versionLabel(v uint16) string {
	switch v {
	case tls.VersionTLS12:
		return "1.2"
	case tls.VersionTLS13:
		return "1.3"
	default:
		return "other"
	}
}

// notifyTLS reports the bounded TLS facts for one Send to the observer.
func (s *clientSession) notifyTLS() {
	o := s.config.TLS.Observer
	if o == nil || s.tls.Outcome == "" {
		return
	}
	version := s.tls.Version
	if version == "" {
		version = "none"
	}
	defer func() { _ = recover() }()
	o.TLSResult(s.tls.Policy.String(), string(s.tls.Outcome), version)
}
