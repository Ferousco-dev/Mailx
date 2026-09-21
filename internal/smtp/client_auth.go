// Package smtp — outbound SMTP AUTH (RFC 4954) for a trusted relay.
//
// Position in the session (client.go run):
//
//	220 -> EHLO -> STARTTLS -> handshake -> EHLO again (post-TLS capabilities)
//	    -> AUTH (this file) -> MAIL FROM -> RCPT TO -> DATA
//
// Invariants (each covered by a test):
//   - Credentials are only ever sent after TLS is established and verified.
//     If credentials are present the client behaves as TLSRequired regardless of
//     the configured policy; there is no plaintext AUTH and no fallback.
//   - The mechanism is chosen ONLY from the AUTH capability of the post-TLS
//     EHLO. A pre-TLS advertisement is never used (RFC 4954: the list may change
//     after STARTTLS).
//   - MAIL FROM is never sent on a session that carries credentials until AUTH
//     succeeded.
//   - Nothing derived from the credentials (username, password, base64 payload)
//     appears in errors, results or observer events, and remote AUTH reply text
//     is discarded (servers occasionally echo it).
//
// Supported mechanisms: PLAIN (RFC 4616, the RFC 4954 mandatory-to-implement
// mechanism) and LOGIN (de-facto; still required by some relays), both only
// over verified TLS. Intentionally unsupported: CRAM-MD5 and DIGEST-MD5
// (obsolete), XOAUTH2/OAUTHBEARER (provider integrations, later), SCRAM
// (rare on submission servers, later if needed).
package smtp

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
)

// Credentials are SMTP AUTH credentials for ONE trusted relay. They are only
// ever attached to a DeliveryRequest by the relay path; the direct MX path
// never has any. Every formatting verb prints a redacted placeholder.
type Credentials struct {
	Username string
	Password string
}

func (Credentials) String() string       { return "smtp.Credentials{redacted}" }
func (Credentials) GoString() string     { return "smtp.Credentials{redacted}" }
func (Credentials) LogValue() slog.Value { return slog.StringValue("[redacted]") }
func (c Credentials) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte("smtp.Credentials{redacted}"))
}

// maxCredentialBytes follows RFC 4616 (each field at most 255 octets).
const maxCredentialBytes = 255

// Validate rejects empty values, over-long values and control characters.
// Errors never contain the values.
func (c Credentials) Validate() error {
	for name, v := range map[string]string{"username": c.Username, "password": c.Password} {
		switch {
		case v == "":
			return fmt.Errorf("SMTP %s is empty", name)
		case len(v) > maxCredentialBytes:
			return fmt.Errorf("SMTP %s exceeds %d bytes", name, maxCredentialBytes)
		case strings.ContainsAny(v, "\r\n\x00"):
			return fmt.Errorf("SMTP %s contains a forbidden control character", name)
		}
	}
	return nil
}

// AuthOutcome is the bounded set of AUTH results, safe as a metric label.
type AuthOutcome string

const (
	AuthSuccess       AuthOutcome = "success"
	AuthRejected      AuthOutcome = "rejected"  // 5xx: credentials or policy refused
	AuthTemporary     AuthOutcome = "temporary" // 4xx: server-side trouble
	AuthNoTLS         AuthOutcome = "no_tls"    // guard: never reached with a correct TLS layer
	AuthNotAdvertised AuthOutcome = "not_advertised"
	AuthNoMechanism   AuthOutcome = "no_mechanism"
	AuthProtocolError AuthOutcome = "protocol_error"
	AuthConnLost      AuthOutcome = "connection_lost"
	AuthTimeout       AuthOutcome = "timeout"
	AuthCanceled      AuthOutcome = "canceled"
)

// AuthInfo describes the AUTH decision for one Send. Mechanism is "plain",
// "login" or "" (none chosen). It carries no credential-derived data.
type AuthInfo struct {
	Attempted bool
	Mechanism string
	Outcome   AuthOutcome
}

// AuthObserver receives one bounded event per Send that carried credentials.
// mechanism is "plain", "login" or "none"; outcome is an AuthOutcome value.
type AuthObserver interface {
	AuthResult(mechanism, outcome string)
}

// authMechanisms lists the SASL mechanisms from the latest EHLO, accepting both
// "AUTH PLAIN LOGIN" and the legacy "AUTH=PLAIN LOGIN" spellings.
func (c capabilities) authMechanisms() []string {
	var out []string
	for key, val := range c {
		switch {
		case key == "AUTH":
			out = append(out, strings.Fields(strings.ToUpper(val))...)
		case strings.HasPrefix(key, "AUTH="):
			out = append(out, strings.TrimPrefix(key, "AUTH="))
			out = append(out, strings.Fields(strings.ToUpper(val))...)
		}
	}
	return out
}

func chooseMechanism(advertised []string) string {
	have := map[string]bool{}
	for _, m := range advertised {
		have[strings.ToUpper(m)] = true
	}
	for _, m := range []string{"PLAIN", "LOGIN"} { // preference order
		if have[m] {
			return m
		}
	}
	return ""
}

// authenticate runs AUTH on a session that has credentials. It is a no-op
// without credentials.
func (s *clientSession) authenticate(ctx context.Context) *DeliveryError {
	if s.creds == nil {
		return nil
	}
	if !s.tls.Established { // defense in depth: secure() already refuses this
		return s.authFail(AuthNoTLS, 0, "", nil)
	}
	advertised := s.caps.authMechanisms()
	if len(advertised) == 0 {
		return s.authFail(AuthNotAdvertised, 0, "", nil)
	}
	mech := chooseMechanism(advertised)
	if mech == "" {
		return s.authFail(AuthNoMechanism, 0, "", nil)
	}
	s.auth.Attempted = true
	s.auth.Mechanism = strings.ToLower(mech)
	if mech == "PLAIN" {
		return s.authPlain(ctx)
	}
	return s.authLogin(ctx)
}

func (s *clientSession) authPlain(ctx context.Context) *DeliveryError {
	payload := b64("\x00" + s.creds.Username + "\x00" + s.creds.Password)
	// RFC 4954: do not use the initial response if it would exceed the command
	// line limit; fall back to the challenge form.
	if line := "AUTH PLAIN " + payload; len(line)+2 <= s.config.MaxReplyLineBytes {
		return s.authFinish(s.authRound(ctx, line))
	}
	code, enh, derr := s.authRound(ctx, "AUTH PLAIN")
	if derr != nil {
		return derr
	}
	if code != 334 {
		return s.authFinish(code, enh, nil)
	}
	return s.authFinish(s.authRound(ctx, payload))
}

func (s *clientSession) authLogin(ctx context.Context) *DeliveryError {
	for i, line := range []string{"AUTH LOGIN", b64(s.creds.Username), b64(s.creds.Password)} {
		code, enh, derr := s.authRound(ctx, line)
		if derr != nil {
			return derr
		}
		if i < 2 {
			if code != 334 { // any other reply here is a refusal or a protocol violation
				return s.authFinish(code, enh, nil)
			}
			continue
		}
		return s.authFinish(code, enh, nil)
	}
	return s.authFail(AuthProtocolError, 0, "", nil)
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// authRound sends one AUTH line and reads the reply. The reply TEXT is dropped:
// only the code and enhanced status are kept.
func (s *clientSession) authRound(ctx context.Context, line string) (int, string, *DeliveryError) {
	code, enh, _, err := s.command(line)
	if err != nil {
		return 0, "", s.authFail(classifyAuthIO(ctx, err), 0, "", err)
	}
	return code, enh, nil
}

// authFinish maps the reply that ends the exchange to a result.
func (s *clientSession) authFinish(code int, enh string, derr *DeliveryError) *DeliveryError {
	if derr != nil {
		return derr
	}
	switch {
	case code == 235:
		s.auth.Outcome = AuthSuccess
		return nil
	case code >= 400 && code < 500:
		return s.authFail(AuthTemporary, code, enh, nil)
	case code >= 500 && code < 600:
		return s.authFail(AuthRejected, code, enh, nil)
	default:
		return s.authFail(AuthProtocolError, code, "", nil)
	}
}

// authFail records the outcome and builds the DeliveryError. Remote text is
// intentionally not kept. Temporary follows SMTP semantics (4xx and transport
// trouble); the delivery engine decides what to do with a relay failure.
func (s *clientSession) authFail(outcome AuthOutcome, code int, enh string, cause error) *DeliveryError {
	s.auth.Outcome = outcome
	return &DeliveryError{
		Stage: StageAuth, Code: code, Enhanced: enh,
		Temporary: outcome == AuthTemporary || outcome == AuthConnLost || outcome == AuthTimeout || outcome == AuthCanceled,
		Err:       &authFailure{outcome: outcome, cause: cause},
	}
}

// authFailure is the bounded error under an AUTH DeliveryError. Its text is the
// outcome category only; a transport cause is reachable through Unwrap.
type authFailure struct {
	outcome AuthOutcome
	cause   error
}

func (e *authFailure) Error() string { return "auth " + string(e.outcome) }
func (e *authFailure) Unwrap() error { return e.cause }

func classifyAuthIO(ctx context.Context, err error) AuthOutcome {
	switch {
	case ctx.Err() != nil:
		return AuthCanceled
	case isTimeout(err):
		return AuthTimeout
	case isConnLost(err):
		return AuthConnLost
	default:
		return AuthProtocolError
	}
}

// notifyAuth reports the bounded AUTH facts for one Send to the observer.
func (s *clientSession) notifyAuth() {
	o := s.config.AuthObserver
	if o == nil || s.creds == nil || s.auth.Outcome == "" {
		return
	}
	mech := s.auth.Mechanism
	if mech == "" {
		mech = "none"
	}
	defer func() { _ = recover() }()
	o.AuthResult(mech, string(s.auth.Outcome))
}
