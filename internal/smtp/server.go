package smtp

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
)

type state uint8

const (
	connected state = iota
	greeted
	mailReceived
)

var errCommandTooLong = errors.New("SMTP command line too long")
var errInvalidCommandFraming = errors.New("invalid SMTP command line framing")

type dataReadResult struct {
	raw       string
	oversized bool
	malformed bool
}

type Session struct {
	// ID is a process-local random session identifier for log correlation.
	ID            string
	state         state
	Envelope      mail.Envelope
	Authenticated bool
	TenantID      string
}

func (s *Session) resetTransaction() {
	s.Envelope = mail.Envelope{}
	if s.state == mailReceived {
		s.state = greeted
	}
}

type connection struct {
	conn   net.Conn
	config Config
	reader *bufio.Reader
	id     string
}

func HandleConnection(conn net.Conn, sink func(Session, mail.Message) error) {
	_ = HandleConnectionWithConfig(conn, DefaultConfig(), sink)
}

// HandleConnectionWithConfig serves one connection using instance-scoped
// limits. It always closes conn before returning.
func HandleConnectionWithConfig(conn net.Conn, config Config, sink func(Session, mail.Message) error) error {
	defer conn.Close()
	var err error
	config, err = config.normalized()
	if err != nil {
		return err
	}
	c := connection{conn: conn, config: config, reader: bufio.NewReaderSize(conn, config.CommandLineLimit), id: newSessionID()}
	start := time.Now()
	if o := config.Observer; o != nil {
		observe(func() { o.SessionStarted(c.id) })
	}
	err = c.serve(sink)
	if o := config.Observer; o != nil {
		observe(func() { o.SessionEnded(c.id, time.Since(start), err != nil) })
	}
	return err
}

func (c *connection) serve(sink func(Session, mail.Message) error) error {
	s := Session{state: connected, ID: c.id}
	if err := c.reply(standardReply(220, "localhost MailX SMTP Server")); err != nil {
		return err
	}
	for {
		line, err := c.readCommand()
		if errors.Is(err, errCommandTooLong) {
			return c.reply(enhancedReply(500, statusSyntaxError, "Line too long"))
		}
		if errors.Is(err, errInvalidCommandFraming) {
			if err = c.reply(enhancedReply(500, statusSyntaxError, "Syntax error, command unrecognized")); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return nil
		}
		cmd, arg, hasArgument, validSyntax := command(line)
		if !validSyntax {
			if cmd == "" || !isKnownCommand(cmd) {
				err = c.reply(enhancedReply(500, statusSyntaxError, "Syntax error, command unrecognized"))
			} else {
				err = c.reply(enhancedReply(501, statusInvalidArguments, "Syntax error in parameters or arguments"))
			}
			if err != nil {
				return err
			}
			continue
		}
		switch cmd {
		case "EHLO", "HELO":
			if !hasArgument || !validGreetingArgument(arg) {
				// RFC 2034 excludes all HELO and EHLO responses from enhanced codes.
				err = c.reply(standardReply(501, "Syntax: "+cmd+" hostname"))
				break
			}
			s.resetTransaction()
			s.state = greeted
			if cmd == "EHLO" {
				if c.config.RequireAuth {
					err = c.reply(multilineReply(250, "localhost", "ENHANCEDSTATUSCODES", "AUTH PLAIN"))
				} else {
					err = c.reply(multilineReply(250, "localhost", "ENHANCEDSTATUSCODES"))
				}
			} else {
				err = c.reply(standardReply(250, "localhost"))
			}
		case "NOOP":
			if hasArgument && strings.TrimSpace(arg) == "" {
				err = c.reply(enhancedReply(501, statusInvalidArguments, "Syntax: NOOP [string]"))
				break
			}
			err = c.reply(enhancedReply(250, statusOtherSuccess, "OK"))
		case "RSET":
			if hasArgument {
				err = c.reply(enhancedReply(501, statusInvalidArguments, "Syntax: RSET"))
				break
			}
			s.resetTransaction()
			err = c.reply(enhancedReply(250, statusOtherSuccess, "OK"))
		case "AUTH":
			if !hasArgument {
				err = c.reply(enhancedReply(501, statusInvalidArguments, "Syntax: AUTH PLAIN <response>"))
				break
			}
			mech, resp, _ := strings.Cut(arg, " ")
			if !strings.EqualFold(mech, "PLAIN") || resp == "" {
				err = c.reply(enhancedReply(504, statusInvalidCommand, "Unrecognized authentication mechanism"))
				break
			}
			raw, decErr := base64.StdEncoding.DecodeString(resp)
			parts := bytes.SplitN(raw, []byte{0}, 3)
			if decErr != nil || len(parts) != 3 {
				err = c.reply(enhancedReply(501, statusInvalidArguments, "Invalid AUTH PLAIN response"))
				break
			}
			if c.config.Authenticator == nil {
				err = c.reply(enhancedReply(535, statusInvalidCommand, "Authentication not available"))
				break
			}
			tenantID, ok := c.config.Authenticator(string(parts[1]), string(parts[2]))
			if !ok {
				err = c.reply(enhancedReply(535, statusInvalidCommand, "Authentication credentials invalid"))
				break
			}
			s.Authenticated = true
			s.TenantID = tenantID
			err = c.reply(enhancedReply(235, statusOtherSuccess, "Authentication successful"))
		case "MAIL":
			if c.config.RequireAuth && !s.Authenticated {
				err = c.reply(enhancedReply(530, statusInvalidCommand, "Authentication required"))
				break
			}
			path, valid := parsePathArgument(arg, hasArgument, "FROM", true)
			if !valid {
				err = c.reply(enhancedReply(501, statusInvalidArguments, "Syntax: MAIL FROM:<address>"))
				break
			}
			if s.state != greeted {
				err = c.reply(enhancedReply(503, statusInvalidCommand, "Bad sequence of commands"))
				break
			}
			s.Envelope.MailFrom = path
			s.state = mailReceived
			err = c.reply(enhancedReply(250, statusSenderAccepted, "Sender OK"))
		case "RCPT":
			path, valid := parsePathArgument(arg, hasArgument, "TO", false)
			if !valid {
				err = c.reply(enhancedReply(501, statusInvalidArguments, "Syntax: RCPT TO:<address>"))
				break
			}
			if s.state != mailReceived {
				err = c.reply(enhancedReply(503, statusInvalidCommand, "Bad sequence of commands"))
				break
			}
			if len(s.Envelope.Recipients) >= c.config.MaxRecipients {
				err = c.reply(enhancedReply(452, statusTooManyRecipients, "Too many recipients"))
				break
			}
			s.Envelope.Recipients = append(s.Envelope.Recipients, path)
			err = c.reply(enhancedReply(250, statusRecipientAccepted, "Recipient OK"))
		case "DATA":
			if hasArgument {
				err = c.reply(enhancedReply(501, statusInvalidArguments, "Syntax: DATA"))
				break
			}
			if s.state != mailReceived || len(s.Envelope.Recipients) == 0 {
				err = c.reply(enhancedReply(503, statusInvalidCommand, "Bad sequence of commands"))
				break
			}
			if err = c.reply(standardReply(354, "End data with <CR><LF>.<CR><LF>")); err != nil {
				break
			}
			var result dataReadResult
			result, err = c.readData()
			if err != nil {
				return nil
			}
			if result.oversized {
				s.resetTransaction()
				c.message("rejected", "oversized")
				err = c.reply(enhancedReply(552, statusMessageTooLarge, "Message size exceeds fixed limit"))
				break
			}
			if result.malformed {
				s.resetTransaction()
				c.message("rejected", "malformed")
				err = c.reply(enhancedReply(554, statusMessageContentError, "Malformed mail data"))
				break
			}
			var message mail.Message
			message, err = mail.ParseMessage(result.raw)
			if err != nil {
				s.resetTransaction()
				c.message("rejected", "content")
				err = c.reply(enhancedReply(554, statusMessageContentError, "Message content rejected"))
				break
			}
			if sink != nil {
				if sinkErr := sink(s, message); sinkErr != nil {
					s.resetTransaction()
					c.message("temporary_failure", "sink_failed")
					err = c.reply(enhancedReply(451, statusTemporarySystem, "Temporary internal failure"))
					break
				}
			}
			s.resetTransaction()
			c.message("accepted", "")
			err = c.reply(enhancedReply(250, statusMessageAccepted, "Message accepted by MailX"))
		case "QUIT":
			if hasArgument {
				err = c.reply(enhancedReply(501, statusInvalidArguments, "Syntax: QUIT"))
				break
			}
			return c.reply(enhancedReply(221, statusOtherSuccess, "Bye"))
		default:
			err = c.reply(enhancedReply(500, statusSyntaxError, "Command unrecognized"))
		}
		if err != nil {
			return err
		}
	}
}

func (c *connection) readCommand() (string, error) {
	if err := c.refreshReadDeadline(); err != nil {
		return "", err
	}
	line, err := c.reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > c.config.CommandLineLimit {
		return "", errCommandTooLong
	}
	if err != nil {
		return "", err
	}
	if !hasValidCRLF(line) || bytes.IndexByte(line[:len(line)-2], '\r') >= 0 {
		return "", errInvalidCommandFraming
	}
	return string(line[:len(line)-2]), nil
}

func (c *connection) readData() (dataReadResult, error) {
	var body strings.Builder
	var line []byte
	var size int64
	oversized := false
	malformed := false
	atLineStart := true
	for {
		if err := c.refreshReadDeadline(); err != nil {
			return dataReadResult{}, err
		}
		fragment, err := c.reader.ReadSlice('\n')
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return dataReadResult{}, err
		}
		completeLine := err == nil

		// The DATA terminator is framing, not message content. It remains
		// recognizable while discarding an oversized message so the session can
		// be resynchronized without retaining discarded bytes.
		if atLineStart && completeLine && isDataTerminator(fragment) {
			return dataReadResult{raw: body.String(), oversized: oversized, malformed: malformed}, nil
		}
		if oversized || malformed {
			atLineStart = completeLine
			continue
		}

		// A dot-stuffed line is one byte larger on the wire than the canonical
		// content MailX stores, so permit one byte of temporary framing overhead.
		remaining := c.config.MaxMessageSize - size
		maxRawLine := remaining
		if maxRawLine < int64(^uint64(0)>>1) {
			maxRawLine++
		}
		fragmentSize := int64(len(fragment))
		if fragmentSize > maxRawLine || int64(len(line)) > maxRawLine-fragmentSize {
			oversized = true
			line = nil
			atLineStart = completeLine
			continue
		}
		line = append(line, fragment...)
		if !completeLine {
			atLineStart = false
			continue
		}

		if !hasValidCRLF(line) || bytes.IndexByte(line[:len(line)-2], '\r') >= 0 {
			malformed = true
			line = nil
			atLineStart = true
			continue
		}
		line = line[:len(line)-2]
		if len(line) > 0 && line[0] == '.' {
			line = line[1:]
		}
		lineSize := int64(len(line)) + 2
		if lineSize > remaining {
			oversized = true
			line = nil
			atLineStart = true
			continue
		}
		size += lineSize
		body.Write(line)
		body.WriteString("\r\n")
		line = nil
		atLineStart = true
	}
}

func isDataTerminator(line []byte) bool {
	return bytes.Equal(line, []byte(".\r\n"))
}

func hasValidCRLF(line []byte) bool {
	return len(line) >= 2 && line[len(line)-2] == '\r' && line[len(line)-1] == '\n'
}

func (c *connection) refreshReadDeadline() error {
	if c.config.ReadTimeout == 0 {
		return c.conn.SetReadDeadline(time.Time{})
	}
	return c.conn.SetReadDeadline(time.Now().Add(c.config.ReadTimeout))
}

func (c *connection) reply(response reply) error {
	if c.config.WriteTimeout == 0 {
		if err := c.conn.SetWriteDeadline(time.Time{}); err != nil {
			return err
		}
	} else if err := c.conn.SetWriteDeadline(time.Now().Add(c.config.WriteTimeout)); err != nil {
		return err
	}
	wire, err := response.wire()
	if err != nil {
		return err
	}
	remaining := []byte(wire)
	for len(remaining) > 0 {
		written, writeErr := c.conn.Write(remaining)
		if writeErr != nil {
			return writeErr
		}
		if written <= 0 || written > len(remaining) {
			return io.ErrShortWrite
		}
		remaining = remaining[written:]
	}
	return nil
}

func command(line string) (verb, argument string, hasArgument, valid bool) {
	if line == "" || line[0] == ' ' || line[0] == '\t' {
		return "", "", false, false
	}
	for index := 0; index < len(line); index++ {
		if line[index] == ' ' {
			break
		}
		if line[index] < 0x20 || line[index] > 0x7e {
			return strings.ToUpper(line[:index]), "", true, false
		}
	}

	separator := strings.IndexByte(line, ' ')
	if separator < 0 {
		verb = strings.ToUpper(line)
		return verb, "", false, isASCIICommandText(line)
	}
	verb = strings.ToUpper(line[:separator])
	if separator+1 >= len(line) || line[separator+1] == ' ' {
		return verb, "", true, false
	}
	argument = line[separator+1:]
	return verb, argument, true, isASCIICommandText(line)
}

func isKnownCommand(command string) bool {
	switch command {
	case "EHLO", "HELO", "MAIL", "RCPT", "DATA", "RSET", "NOOP", "QUIT", "AUTH":
		return true
	default:
		return false
	}
}

func isASCIICommandText(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] < 0x20 || value[index] > 0x7e {
			return false
		}
	}
	return true
}

func validGreetingArgument(argument string) bool {
	return argument != "" && !strings.ContainsAny(argument, " \t")
}

func parsePathArgument(argument string, hasArgument bool, keyword string, allowEmpty bool) (string, bool) {
	prefix := keyword + ":<"
	if !hasArgument || len(argument) < len(prefix)+1 || !strings.EqualFold(argument[:len(prefix)], prefix) || argument[len(argument)-1] != '>' {
		return "", false
	}
	path := argument[len(prefix)-1:]
	mailbox := path[1 : len(path)-1]
	if (!allowEmpty && mailbox == "") || strings.ContainsAny(mailbox, " <>\t") || !isASCIICommandText(mailbox) {
		return "", false
	}
	return path, true
}
