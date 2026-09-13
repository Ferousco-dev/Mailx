package smtp

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/Ferousco-dev/mailx/internal/mail"
)

type state uint8

const (
	connected state = iota
	greeted
	mailReceived
)

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

func HandleConnection(conn net.Conn, sink func(Session, mail.Message)) {
	defer conn.Close()
	s := Session{state: connected}
	r := bufio.NewReader(conn)
	reply(conn, "220 localhost MailX SMTP Server")
	for {
		line, e := r.ReadString('\n')
		if e != nil {
			return
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		cmd, arg := command(line)
		switch cmd {
		case "EHLO", "HELO":
			// A new greeting starts a fresh SMTP transaction.
			s.resetTransaction()
			s.state = greeted
			if cmd == "EHLO" {
				fmt.Fprint(conn, "250-localhost\r\n250 OK\r\n")
			} else {
				reply(conn, "250 localhost")
			}
		case "NOOP":
			reply(conn, "250 OK")
		case "RSET":
			s.resetTransaction()
			reply(conn, "250 OK")
		case "MAIL":
			if s.state != greeted {
				reply(conn, "503 Bad sequence of commands")
				continue
			}
			if !strings.HasPrefix(strings.ToUpper(arg), "FROM:") {
				reply(conn, "501 Syntax: MAIL FROM:<address>")
				continue
			}
			s.Envelope.MailFrom = strings.TrimSpace(arg[5:])
			if s.Envelope.MailFrom == "" {
				reply(conn, "501 Syntax: MAIL FROM:<address>")
				continue
			}
			s.state = mailReceived
			reply(conn, "250 OK")
		case "RCPT":
			if s.state != mailReceived {
				reply(conn, "503 Bad sequence of commands")
				continue
			}
			if !strings.HasPrefix(strings.ToUpper(arg), "TO:") || strings.TrimSpace(arg[3:]) == "" {
				reply(conn, "501 Syntax: RCPT TO:<address>")
				continue
			}
			s.Envelope.Recipients = append(s.Envelope.Recipients, strings.TrimSpace(arg[3:]))
			reply(conn, "250 OK")
		case "DATA":
			if s.state != mailReceived || len(s.Envelope.Recipients) == 0 {
				reply(conn, "503 Bad sequence of commands")
				continue
			}
			reply(conn, "354 End data with <CR><LF>.<CR><LF>")
			raw, ok := data(r)
			if !ok {
				return
			}
			message, err := mail.ParseMessage(raw)
			if err != nil {
				reply(conn, "554 Message content rejected")
				s.resetTransaction()
				continue
			}
			if sink != nil {
				sink(s, message)
			}
			reply(conn, "250 Message accepted by MailX")
			s.resetTransaction()
		case "QUIT":
			reply(conn, "221 Bye")
			return
		default:
			reply(conn, "500 Command unrecognized")
		}
	}
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
	command := strings.ToUpper(fields[0])
	argumentStart := strings.IndexFunc(line, func(r rune) bool { return r == ' ' || r == '\t' })
	return command, strings.TrimSpace(line[argumentStart:])
}
func reply(w io.Writer, s string) { fmt.Fprintf(w, "%s\r\n", s) }
func data(r *bufio.Reader) (string, bool) {
	var b strings.Builder
	for {
		line, e := r.ReadString('\n')
		if e != nil {
			return "", false
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "." {
			return b.String(), true
		}
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}
		b.WriteString(line)
		b.WriteString("\r\n")
	}
}
