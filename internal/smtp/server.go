package smtp

import (
	"bufio"
	"errors"
	"fmt"
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

type Session struct {
	state    state
	Envelope mail.Envelope
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
	c := connection{conn: conn, config: config, reader: bufio.NewReaderSize(conn, config.CommandLineLimit)}
	return c.serve(sink)
}

func (c *connection) serve(sink func(Session, mail.Message) error) error {
	s := Session{state: connected}
	if err := c.reply("220 localhost MailX SMTP Server"); err != nil {
		return err
	}
	for {
		line, err := c.readCommand()
		if errors.Is(err, errCommandTooLong) {
			return c.reply("500 Line too long")
		}
		if err != nil {
			return nil
		}
		cmd, arg := command(line)
		switch cmd {
		case "EHLO", "HELO":
			s.resetTransaction()
			s.state = greeted
			if cmd == "EHLO" {
				err = c.reply("250-localhost\r\n250 OK")
			} else {
				err = c.reply("250 localhost")
			}
		case "NOOP":
			err = c.reply("250 OK")
		case "RSET":
			s.resetTransaction()
			err = c.reply("250 OK")
		case "MAIL":
			if s.state != greeted {
				err = c.reply("503 Bad sequence of commands")
				break
			}
			if !strings.HasPrefix(strings.ToUpper(arg), "FROM:") || strings.TrimSpace(arg[5:]) == "" {
				err = c.reply("501 Syntax: MAIL FROM:<address>")
				break
			}
			s.Envelope.MailFrom = strings.TrimSpace(arg[5:])
			s.state = mailReceived
			err = c.reply("250 OK")
		case "RCPT":
			if s.state != mailReceived {
				err = c.reply("503 Bad sequence of commands")
				break
			}
			if !strings.HasPrefix(strings.ToUpper(arg), "TO:") || strings.TrimSpace(arg[3:]) == "" {
				err = c.reply("501 Syntax: RCPT TO:<address>")
				break
			}
			s.Envelope.Recipients = append(s.Envelope.Recipients, strings.TrimSpace(arg[3:]))
			err = c.reply("250 OK")
		case "DATA":
			if s.state != mailReceived || len(s.Envelope.Recipients) == 0 {
				err = c.reply("503 Bad sequence of commands")
				break
			}
			if arg != "" {
				err = c.reply("501 Syntax: DATA")
				break
			}
			if err = c.reply("354 End data with <CR><LF>.<CR><LF>"); err != nil {
				break
			}
			var raw string
			var oversized bool
			raw, oversized, err = c.readData()
			if err != nil {
				return nil
			}
			if oversized {
				s.resetTransaction()
				err = c.reply("552 Too much mail data")
				break
			}
			var message mail.Message
			message, err = mail.ParseMessage(raw)
			if err != nil {
				s.resetTransaction()
				err = c.reply("554 Message content rejected")
				break
			}
			if sink != nil {
				if sinkErr := sink(s, message); sinkErr != nil {
					s.resetTransaction()
					err = c.reply("451 Local storage error")
					break
				}
			}
			s.resetTransaction()
			err = c.reply("250 Message accepted by MailX")
		case "QUIT":
			return c.reply("221 Bye")
		default:
			err = c.reply("500 Command unrecognized")
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
	return strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r"), nil
}

func (c *connection) readData() (string, bool, error) {
	var body strings.Builder
	var line []byte
	var size int64
	oversized := false
	atLineStart := true
	for {
		if err := c.refreshReadDeadline(); err != nil {
			return "", oversized, err
		}
		fragment, err := c.reader.ReadSlice('\n')
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return "", oversized, err
		}
		completeLine := err == nil

		// The DATA terminator is framing, not message content. It remains
		// recognizable while discarding an oversized message so the session can
		// be resynchronized without retaining discarded bytes.
		if atLineStart && completeLine && isDataTerminator(fragment) {
			return body.String(), oversized, nil
		}
		if oversized {
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

		line = bytesTrimLineEnding(line)
		if len(line) > 1 && line[0] == '.' && line[1] == '.' {
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
	return string(line) == ".\r\n" || string(line) == ".\n"
}

func bytesTrimLineEnding(line []byte) []byte {
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return line
}

func (c *connection) refreshReadDeadline() error {
	if c.config.ReadTimeout == 0 {
		return c.conn.SetReadDeadline(time.Time{})
	}
	return c.conn.SetReadDeadline(time.Now().Add(c.config.ReadTimeout))
}

func (c *connection) reply(message string) error {
	if c.config.WriteTimeout == 0 {
		if err := c.conn.SetWriteDeadline(time.Time{}); err != nil {
			return err
		}
	} else if err := c.conn.SetWriteDeadline(time.Now().Add(c.config.WriteTimeout)); err != nil {
		return err
	}
	_, err := fmt.Fprintf(c.conn, "%s\r\n", message)
	return err
}

func command(line string) (string, string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", ""
	}
	fields := strings.Fields(line)
	if len(fields) == 1 {
		return strings.ToUpper(fields[0]), ""
	}
	argumentStart := strings.IndexFunc(line, func(r rune) bool { return r == ' ' || r == '\t' })
	return strings.ToUpper(fields[0]), strings.TrimSpace(line[argumentStart:])
}
