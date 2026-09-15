// Package outbound builds RFC 5322/2045 outbound MIME messages from
// structured input (the reverse of internal/mail's parser, which only
// reads raw MIME MailX already received). It is the API's only path from
// a SendEmail request to bytes that internal/storage and the delivery
// pipeline already know how to handle.
package outbound

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
	"unicode"
)

var (
	ErrEmptyBody   = errors.New("outbound: message has neither text nor html body")
	ErrNoRecipient = errors.New("outbound: message has no recipients")
)

// Request is everything needed to build one outbound message. Bcc is part
// of the SMTP envelope but MUST NOT appear in the built headers.
type Request struct {
	From      string
	To        []string
	Cc        []string
	Bcc       []string
	ReplyTo   string
	Subject   string
	Text      string
	HTML      string
	MessageID string // e.g. "<hex@mailx.local>"; caller supplies so it can match the API's own email ID
	Date      time.Time
}

// Built is the result of Build: Raw is the complete CRLF message ready for
// SMTP DATA, and Envelope carries the true recipient list (To+Cc+Bcc) in
// the "<addr>" bracket form the rest of MailX already uses.
type Built struct {
	Raw      string
	From     string   // bracket-form envelope sender
	Envelope []string // bracket-form envelope recipients, To+Cc+Bcc
	To       []string // bracket-form, role-tagged (for durable recipient storage)
	Cc       []string
	Bcc      []string
}

// Build validates req and constructs an RFC-compliant message. Every
// user-controlled string is rejected outright if it contains CR, LF, or
// NUL — none of them may ever reach a raw header line unescaped.
func Build(req Request) (Built, error) {
	if err := rejectControlChars("from", req.From); err != nil {
		return Built{}, err
	}
	if err := rejectControlChars("reply_to", req.ReplyTo); err != nil {
		return Built{}, err
	}
	if err := rejectControlChars("subject", req.Subject); err != nil {
		return Built{}, err
	}
	if strings.TrimSpace(req.Text) == "" && strings.TrimSpace(req.HTML) == "" {
		return Built{}, ErrEmptyBody
	}

	from, fromEnvelope, err := parseOneAddress("from", req.From)
	if err != nil {
		return Built{}, err
	}
	toHeader, toEnvelope, err := parseAddressList("to", req.To)
	if err != nil {
		return Built{}, err
	}
	ccHeader, ccEnvelope, err := parseAddressList("cc", req.Cc)
	if err != nil {
		return Built{}, err
	}
	// The returned header string is intentionally discarded: Bcc must
	// never be written to a header line.
	_, bccEnvelope, err := parseAddressList("bcc", req.Bcc)
	if err != nil {
		return Built{}, err
	}
	envelope := append(append(append([]string{}, toEnvelope...), ccEnvelope...), bccEnvelope...)
	if len(envelope) == 0 {
		return Built{}, ErrNoRecipient
	}

	var replyTo string
	if strings.TrimSpace(req.ReplyTo) != "" {
		replyTo, _, err = parseOneAddress("reply_to", req.ReplyTo)
		if err != nil {
			return Built{}, err
		}
	}

	date := req.Date
	if date.IsZero() {
		date = time.Now().UTC()
	}

	var body bytes.Buffer
	boundary, err := writeBody(&body, req.Text, req.HTML)
	if err != nil {
		return Built{}, err
	}

	var out strings.Builder
	writeHeader(&out, "From", from)
	if toHeader != "" {
		writeHeader(&out, "To", toHeader)
	}
	if ccHeader != "" {
		writeHeader(&out, "Cc", ccHeader)
	}
	if replyTo != "" {
		writeHeader(&out, "Reply-To", replyTo)
	}
	writeHeader(&out, "Subject", encodeHeaderValue(req.Subject))
	writeHeader(&out, "Date", date.Format(time.RFC1123Z))
	if req.MessageID != "" {
		writeHeader(&out, "Message-ID", req.MessageID)
	}
	writeHeader(&out, "MIME-Version", "1.0")
	if boundary != "" {
		writeHeader(&out, "Content-Type", fmt.Sprintf("multipart/alternative;\r\n\tboundary=%q", boundary))
	} else if req.HTML != "" {
		writeHeader(&out, "Content-Type", "text/html; charset=utf-8")
		writeHeader(&out, "Content-Transfer-Encoding", "base64")
	} else {
		writeHeader(&out, "Content-Type", "text/plain; charset=utf-8")
		writeHeader(&out, "Content-Transfer-Encoding", "base64")
	}
	out.WriteString("\r\n")
	out.WriteString(body.String())

	return Built{
		Raw: out.String(), From: fromEnvelope, Envelope: envelope,
		To: toEnvelope, Cc: ccEnvelope, Bcc: bccEnvelope,
	}, nil
}

// writeBody writes either a single base64 body part (returns boundary "")
// or, when both Text and HTML are present, a multipart/alternative body
// and returns the boundary used in the Content-Type header.
func writeBody(w *bytes.Buffer, text, html string) (string, error) {
	if text != "" && html != "" {
		mw := multipart.NewWriter(w)
		if err := writeAlternativePart(mw, "text/plain", text); err != nil {
			return "", err
		}
		if err := writeAlternativePart(mw, "text/html", html); err != nil {
			return "", err
		}
		if err := mw.Close(); err != nil {
			return "", err
		}
		return mw.Boundary(), nil
	}
	content := text
	if html != "" {
		content = html
	}
	w.WriteString(base64Wrap(content))
	return "", nil
}

func writeAlternativePart(mw *multipart.Writer, contentType, content string) error {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Type", contentType+"; charset=utf-8")
	h.Set("Content-Transfer-Encoding", "base64")
	w, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = w.Write([]byte(base64Wrap(content)))
	return err
}

func writeHeader(out *strings.Builder, name, value string) {
	out.WriteString(name)
	out.WriteString(": ")
	out.WriteString(value)
	out.WriteString("\r\n")
}

func rejectControlChars(field, value string) error {
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("outbound: %s contains a forbidden control character", field)
	}
	return nil
}

// parseOneAddress validates a single RFC 5322 mailbox and returns both the
// full header form ("Name <addr>", RFC 2047-safe) and the bracket-only
// envelope form ("<addr>").
func parseOneAddress(field, raw string) (header, envelope string, err error) {
	if strings.TrimSpace(raw) == "" {
		return "", "", fmt.Errorf("outbound: %s is required", field)
	}
	addr, err := mail.ParseAddress(raw)
	if err != nil {
		return "", "", fmt.Errorf("outbound: %s is not a valid address: %w", field, err)
	}
	return formatAddress(addr), "<" + addr.Address + ">", nil
}

// parseAddressList validates a list of mailboxes and returns a
// comma-joined header value plus each entry's bracket-only envelope form.
func parseAddressList(field string, raw []string) (header string, envelope []string, err error) {
	if len(raw) == 0 {
		return "", nil, nil
	}
	headers := make([]string, 0, len(raw))
	for _, one := range raw {
		if err := rejectControlChars(field, one); err != nil {
			return "", nil, err
		}
		h, e, err := parseOneAddress(field, one)
		if err != nil {
			return "", nil, err
		}
		headers = append(headers, h)
		envelope = append(envelope, e)
	}
	return strings.Join(headers, ", "), envelope, nil
}

func formatAddress(addr *mail.Address) string {
	if addr.Name == "" {
		return "<" + addr.Address + ">"
	}
	if isASCII(addr.Name) {
		return (&mail.Address{Name: addr.Name, Address: addr.Address}).String()
	}
	return mime.QEncoding.Encode("UTF-8", addr.Name) + " <" + addr.Address + ">"
}

func encodeHeaderValue(s string) string {
	if isASCII(s) {
		return s
	}
	return mime.QEncoding.Encode("UTF-8", s)
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return false
		}
	}
	return true
}

// base64Wrap encodes content and hard-wraps it at 76 columns with CRLF, as
// RFC 2045 requires for base64 body content.
func base64Wrap(content string) string {
	const lineLen = 76
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	var out strings.Builder
	for i := 0; i < len(encoded); i += lineLen {
		end := min(i+lineLen, len(encoded))
		out.WriteString(encoded[i:end])
		out.WriteString("\r\n")
	}
	return out.String()
}

// NewMessageID generates a random Message-ID local part for callers that
// do not want to derive it from their own MailX ID.
func NewMessageID(domain string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("outbound: generate message id: %w", err)
	}
	return "<" + hex.EncodeToString(b) + "@" + domain + ">", nil
}
