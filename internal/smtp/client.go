// Package smtp — outbound client orchestration.
//
// This file owns the SMTP conversation flow only:
//
//	connect → 220 → EHLO (HELO fallback) → MAIL FROM → RCPT TO →
//	DATA → 354 → body → final response → QUIT.
//
// Focused helpers live in sibling files:
//   - client_config.go   : ClientConfig, defaults, normalization
//   - client_reply.go    : readReply, parseReplyLine, splitEnhanced
//   - client_data.go     : serializeForDATA (CRLF + dot-stuffing + terminator)
//   - client_error.go    : Stage, DeliveryError, IsTemporary, replyError
package smtp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
)

// DeliveryRequest names a single outbound SMTP hop.
type DeliveryRequest struct {
	// Address is a host:port suitable for net.Dial. No DNS/MX is performed.
	Address string
	// Envelope carries MAIL FROM (allowing null reverse path) and RCPT TO
	// recipients. Envelope recipients are used directly; message headers are
	// never inspected for routing.
	Envelope mail.Envelope
	// Raw is the RFC 5322 message content to transmit as SMTP DATA. The
	// client normalizes bare LF to CRLF, dot-stuffs any leading dot, and
	// appends the ".\r\n" terminator.
	Raw string
}

// DeliveryResult reports what happened. Accepted is true only after the
// remote server returned a 2xx reply to the DATA terminator. QuitError is
// set when the transaction succeeded but the QUIT exchange failed — the
// message is still considered accepted in that case.
type DeliveryResult struct {
	Accepted     bool
	FinalCode    int
	FinalMessage string
	QuitError    error
}

// Client delivers one message per Send call over a fresh TCP connection.
// A zero Client is not usable; construct with NewClient.
type Client struct {
	config ClientConfig
	dialer func(ctx context.Context, network, address string) (net.Conn, error)
}

// NewClient validates config and returns a ready client.
func NewClient(config ClientConfig) (*Client, error) {
	normalized, err := config.normalized()
	if err != nil {
		return nil, err
	}
	return &Client{
		config: normalized,
		dialer: (&net.Dialer{}).DialContext,
	}, nil
}

// Send performs one outbound SMTP transaction. It is safe for concurrent use.
func (c *Client) Send(ctx context.Context, req DeliveryRequest) (DeliveryResult, error) {
	if req.Address == "" {
		return DeliveryResult{}, &DeliveryError{Stage: StageInvalidInput, Err: errors.New("destination address is empty")}
	}
	if err := validateEnvelopePath(req.Envelope.MailFrom, true); err != nil {
		return DeliveryResult{}, &DeliveryError{Stage: StageInvalidInput, Err: err}
	}
	if len(req.Envelope.Recipients) == 0 {
		return DeliveryResult{}, &DeliveryError{Stage: StageInvalidInput, Err: errors.New("envelope has no recipients")}
	}
	for _, r := range req.Envelope.Recipients {
		if err := validateEnvelopePath(r, false); err != nil {
			return DeliveryResult{}, &DeliveryError{Stage: StageInvalidInput, Recipient: r, Err: err}
		}
	}

	dialCtx, cancelDial := context.WithTimeout(ctx, c.config.DialTimeout)
	conn, err := c.dialer(dialCtx, "tcp", req.Address)
	cancelDial()
	if err != nil {
		return DeliveryResult{}, &DeliveryError{Stage: StageDial, Err: err}
	}
	session := &clientSession{conn: conn, config: c.config, reader: bufio.NewReaderSize(conn, c.config.MaxReplyLineBytes*2)}
	defer session.close()

	// Propagate context cancellation to the socket so blocked reads/writes
	// return promptly.
	stopWatch := session.watchContext(ctx)
	defer stopWatch()

	return session.run(req)
}

// clientSession owns one outbound SMTP connection. Its reply-parsing methods
// live in client_reply.go so this file stays focused on protocol flow.
type clientSession struct {
	conn   net.Conn
	config ClientConfig
	reader *bufio.Reader
}

func (s *clientSession) close() { _ = s.conn.Close() }

// watchContext arranges for ctx cancellation to unblock the connection by
// setting an immediate deadline. The returned func stops the watcher.
func (s *clientSession) watchContext(ctx context.Context) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = s.conn.SetDeadline(time.Unix(1, 0))
		case <-done:
		}
	}()
	return func() { close(done) }
}

func (s *clientSession) run(req DeliveryRequest) (DeliveryResult, error) {
	// 1) Greeting.
	code, _, msg, err := s.readReply()
	if err != nil {
		return DeliveryResult{}, deliveryErrorFromRead(StageGreeting, err)
	}
	if code != 220 {
		return DeliveryResult{}, replyError(StageGreeting, code, "", msg)
	}

	// 2) EHLO, with HELO fallback on 5xx per RFC 5321 §3.2.
	code, _, msg, err = s.command("EHLO " + s.config.Identity)
	if err != nil {
		return DeliveryResult{}, deliveryErrorFromRead(StageEHLO, err)
	}
	if code != 250 {
		if code >= 500 && code <= 599 {
			code, _, msg, err = s.command("HELO " + s.config.Identity)
			if err != nil {
				return DeliveryResult{}, deliveryErrorFromRead(StageHELO, err)
			}
			if code != 250 {
				return DeliveryResult{}, replyError(StageHELO, code, "", msg)
			}
		} else {
			return DeliveryResult{}, replyError(StageEHLO, code, "", msg)
		}
	}

	// 3) MAIL FROM.
	code, enh, msg, err := s.command("MAIL FROM:" + req.Envelope.MailFrom)
	if err != nil {
		return DeliveryResult{}, deliveryErrorFromRead(StageMailFrom, err)
	}
	if code < 200 || code >= 300 {
		return DeliveryResult{}, replyError(StageMailFrom, code, enh, msg)
	}

	// 4) RCPT TO — one per recipient. All-or-error for v0.7.
	for _, recipient := range req.Envelope.Recipients {
		code, enh, msg, err = s.command("RCPT TO:" + recipient)
		if err != nil {
			return DeliveryResult{}, deliveryErrorFromRead(StageRcptTo, err)
		}
		if code < 200 || code >= 300 {
			e := replyError(StageRcptTo, code, enh, msg)
			e.Recipient = recipient
			return DeliveryResult{}, e
		}
	}

	// 5) DATA — must see 354 before writing any message bytes.
	code, enh, msg, err = s.command("DATA")
	if err != nil {
		return DeliveryResult{}, deliveryErrorFromRead(StageData, err)
	}
	if code != 354 {
		return DeliveryResult{}, replyError(StageData, code, enh, msg)
	}

	// 6) Serialize + dot-stuff + terminate (see client_data.go).
	if err := s.write(serializeForDATA(req.Raw)); err != nil {
		return DeliveryResult{}, &DeliveryError{Stage: StageMessage, Err: err}
	}

	// 7) Final DATA response — the only signal of acceptance.
	code, enh, msg, err = s.readReply()
	if err != nil {
		return DeliveryResult{}, deliveryErrorFromRead(StageDataResponse, err)
	}
	if code < 200 || code >= 300 {
		return DeliveryResult{}, replyError(StageDataResponse, code, enh, msg)
	}

	result := DeliveryResult{Accepted: true, FinalCode: code, FinalMessage: msg}

	// 8) QUIT — failure here does NOT unaccept the message.
	if qCode, _, qMsg, qErr := s.command("QUIT"); qErr != nil {
		result.QuitError = &DeliveryError{Stage: StageQuit, Err: qErr}
	} else if qCode != 221 {
		result.QuitError = replyError(StageQuit, qCode, "", qMsg)
	}
	return result, nil
}

func (s *clientSession) command(line string) (int, string, string, error) {
	if err := s.write(line + "\r\n"); err != nil {
		return 0, "", "", err
	}
	return s.readReply()
}

func (s *clientSession) write(payload string) error {
	if s.config.WriteTimeout > 0 {
		if err := s.conn.SetWriteDeadline(time.Now().Add(s.config.WriteTimeout)); err != nil {
			return err
		}
	}
	buf := []byte(payload)
	for len(buf) > 0 {
		n, err := s.conn.Write(buf)
		if err != nil {
			return err
		}
		buf = buf[n:]
	}
	return nil
}

// validateEnvelopePath is MailX's single owner of envelope-path safety on the
// outbound side: it enforces bracketed form, rejects CR/LF/NUL injection, and
// gates the null reverse path. Anything MailX writes into MAIL FROM or RCPT TO
// passes through here first.
func validateEnvelopePath(path string, allowNull bool) error {
	if strings.ContainsAny(path, "\r\n\x00") {
		return fmt.Errorf("envelope path contains forbidden control character")
	}
	if !strings.HasPrefix(path, "<") || !strings.HasSuffix(path, ">") {
		return fmt.Errorf("envelope path must be angle-bracketed")
	}
	if path == "<>" {
		if allowNull {
			return nil
		}
		return fmt.Errorf("null reverse path not allowed for recipient")
	}
	return nil
}
