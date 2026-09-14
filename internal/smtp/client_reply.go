package smtp

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// readReply parses one SMTP reply, single- or multi-line, and returns the
// final code plus the accumulated text with an optional enhanced-status
// stripped from the LAST line (per RFC 2034). It refreshes the read deadline
// on every continuation line so a slow but progressing server does not
// timeout while a runaway server is still bounded by MaxReplyLines.
func (s *clientSession) readReply() (int, string, string, error) {
	var (
		lastCode int
		enhanced string
		texts    []string
	)
	for i := 0; i < s.config.MaxReplyLines; i++ {
		if s.config.ReadTimeout > 0 {
			if err := s.conn.SetReadDeadline(time.Now().Add(s.config.ReadTimeout)); err != nil {
				return 0, "", "", err
			}
		}
		line, err := readBoundedLine(s.reader, s.config.MaxReplyLineBytes)
		if err != nil {
			return 0, "", "", err
		}
		code, cont, text, perr := parseReplyLine(line)
		if perr != nil {
			return 0, "", "", perr
		}
		if i == 0 {
			lastCode = code
		} else if code != lastCode {
			return 0, "", "", fmt.Errorf("mismatched multiline reply codes: %d vs %d", lastCode, code)
		}
		if !cont {
			enhanced, text = splitEnhanced(code, text)
			texts = append(texts, text)
			return code, enhanced, strings.Join(texts, "\n"), nil
		}
		texts = append(texts, text)
	}
	return 0, "", "", fmt.Errorf("SMTP reply exceeds %d lines", s.config.MaxReplyLines)
}

// readBoundedLine reads until CRLF, capped at max bytes including CRLF.
// LF-only lines are rejected as malformed framing.
func readBoundedLine(r *bufio.Reader, max int) (string, error) {
	var buf strings.Builder
	for buf.Len() < max {
		b, err := r.ReadByte()
		if err != nil {
			if err == io.EOF && buf.Len() > 0 {
				return "", fmt.Errorf("EOF before CRLF in SMTP reply")
			}
			return "", err
		}
		buf.WriteByte(b)
		if b == '\n' {
			s := buf.String()
			if len(s) < 2 || s[len(s)-2] != '\r' {
				return "", fmt.Errorf("SMTP reply framing: missing CR before LF")
			}
			return s[:len(s)-2], nil
		}
	}
	return "", fmt.Errorf("SMTP reply line exceeds %d octets", max)
}

// parseReplyLine parses "NNN[ -]text" into code, continuation flag, and text.
func parseReplyLine(line string) (int, bool, string, error) {
	if len(line) < 3 {
		return 0, false, "", fmt.Errorf("SMTP reply too short: %q", line)
	}
	for i := 0; i < 3; i++ {
		if line[i] < '0' || line[i] > '9' {
			return 0, false, "", fmt.Errorf("SMTP reply code not numeric: %q", line[:3])
		}
	}
	code, _ := strconv.Atoi(line[:3])
	if code < 100 || code > 599 {
		return 0, false, "", fmt.Errorf("SMTP reply code out of range: %d", code)
	}
	if len(line) == 3 {
		return code, false, "", nil
	}
	sep := line[3]
	if sep != ' ' && sep != '-' {
		return 0, false, "", fmt.Errorf("SMTP reply separator invalid: %q", string(sep))
	}
	return code, sep == '-', line[4:], nil
}

// splitEnhanced pulls an "X.Y.Z" enhanced status off the final text line,
// but only when the class digit matches the reply code (RFC 2034/3463).
func splitEnhanced(code int, text string) (string, string) {
	if text == "" {
		return "", text
	}
	sp := strings.IndexByte(text, ' ')
	head := text
	if sp >= 0 {
		head = text[:sp]
	}
	parts := strings.Split(head, ".")
	if len(parts) != 3 {
		return "", text
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 {
			return "", text
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return "", text
			}
		}
	}
	if int(head[0]-'0') != code/100 {
		return "", text
	}
	rest := ""
	if sp >= 0 {
		rest = text[sp+1:]
	}
	return head, rest
}
