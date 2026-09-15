// DSN/MIME generation (RFC 3464). Generate produces a multipart/report
// message: a human-readable text/plain part, a message/delivery-status
// part, and an optional text/rfc822-headers part (headers only, never the
// original body). This file only generates; it never sends anything.
package bounce

import (
	"bytes"
	"errors"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"strings"
	"time"
)

var ErrEmptyDSN = errors.New("bounce: DSN has no recipient status blocks")

// MessageOptions carries the RFC 5322 header data for the notification
// itself (not the original failed message's envelope).
type MessageOptions struct {
	From            string
	To              string
	Subject         string
	MessageID       string
	Date            time.Time
	OriginalHeaders string
}

// Generate serializes dsn into a complete multipart/report MIME message
// using CRLF throughout, ready for SMTP DATA (dot-stuffing is the
// outbound client's job, not this package's).
func Generate(dsn DSN, opts MessageOptions) (string, error) {
	if len(dsn.Recipients) == 0 {
		return "", ErrEmptyDSN
	}
	if err := validateHeaderSafe(opts.From, opts.To, opts.Subject, opts.MessageID); err != nil {
		return "", err
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	if err := writeHumanReadablePart(mw, dsn); err != nil {
		return "", err
	}
	if err := writeDeliveryStatusPart(mw, dsn); err != nil {
		return "", err
	}
	if strings.TrimSpace(opts.OriginalHeaders) != "" {
		if err := writeOriginalHeadersPart(mw, opts.OriginalHeaders); err != nil {
			return "", err
		}
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	var out strings.Builder
	writeHeaderLine(&out, "From", opts.From)
	writeHeaderLine(&out, "To", opts.To)
	subject := opts.Subject
	if subject == "" {
		subject = "Delivery Status Notification (Failure)"
	}
	writeHeaderLine(&out, "Subject", subject)
	if opts.MessageID != "" {
		writeHeaderLine(&out, "Message-ID", opts.MessageID)
	}
	if !opts.Date.IsZero() {
		writeHeaderLine(&out, "Date", opts.Date.Format(time.RFC1123Z))
	}
	writeHeaderLine(&out, "MIME-Version", "1.0")
	// Marks this as automatic so recipients don't auto-reply to it — a
	// second bounce-loop defense alongside the null reverse path.
	writeHeaderLine(&out, "Auto-Submitted", "auto-replied")
	writeHeaderLine(&out, "Content-Type", fmt.Sprintf("multipart/report; report-type=delivery-status;\r\n\tboundary=%q", mw.Boundary()))
	out.WriteString("\r\n")
	out.WriteString(body.String())
	return out.String(), nil
}

func writeHumanReadablePart(mw *multipart.Writer, dsn DSN) error {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Transfer-Encoding", "7bit")
	w, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	var text strings.Builder
	fmt.Fprintf(&text, "This is an automatically generated delivery status notification from %s.\r\n\r\n", dsn.ReportingMTA)
	text.WriteString("MailX was unable to deliver your message to the following recipient(s):\r\n\r\n")
	for _, r := range dsn.Recipients {
		if r.Action != ActionFailed {
			continue
		}
		fmt.Fprintf(&text, "  %s\r\n", sanitizeText(r.FinalRecipient))
		if r.Diagnostic != "" {
			fmt.Fprintf(&text, "    Reason: %s\r\n", sanitizeText(r.Diagnostic))
		}
	}
	_, err = w.Write([]byte(text.String()))
	return err
}

func writeDeliveryStatusPart(mw *multipart.Writer, dsn DSN) error {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Type", "message/delivery-status")
	w, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Reporting-MTA: dns;%s\r\n", sanitizeText(dsn.ReportingMTA))
	if !dsn.ArrivalDate.IsZero() {
		fmt.Fprintf(&b, "Arrival-Date: %s\r\n", dsn.ArrivalDate.Format(time.RFC1123Z))
	}
	b.WriteString("\r\n")
	for i, r := range dsn.Recipients {
		if i > 0 {
			b.WriteString("\r\n")
		}
		fmt.Fprintf(&b, "Final-Recipient: rfc822;%s\r\n", sanitizeText(stripAngleBrackets(r.FinalRecipient)))
		fmt.Fprintf(&b, "Action: %s\r\n", r.Action.String())
		if r.Status != nil && r.Status.Valid() {
			fmt.Fprintf(&b, "Status: %s\r\n", r.Status.String())
		}
		if r.Diagnostic != "" {
			diagType := "smtp"
			if r.SMTPCode == 0 {
				diagType = "x-mailx"
			}
			code := r.Diagnostic
			if r.SMTPCode != 0 {
				code = fmt.Sprintf("%d %s", r.SMTPCode, r.Diagnostic)
			}
			fmt.Fprintf(&b, "Diagnostic-Code: %s; %s\r\n", diagType, sanitizeText(code))
		}
	}
	_, err = w.Write([]byte(b.String()))
	return err
}

func writeOriginalHeadersPart(mw *multipart.Writer, headers string) error {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Type", "text/rfc822-headers")
	w, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = w.Write([]byte(normalizeCRLF(headers)))
	return err
}

func stripAngleBrackets(addr string) string {
	addr = strings.TrimSpace(addr)
	if strings.HasPrefix(addr, "<") && strings.HasSuffix(addr, ">") && len(addr) >= 2 {
		return addr[1 : len(addr)-1]
	}
	return addr
}

// sanitizeText strips CR/LF/NUL so untrusted remote text can never inject
// a DSN field or plain-text line.
func sanitizeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\r' || r == '\n' || r == 0 {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func normalizeCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", "\r\n")
	return s
}

func validateHeaderSafe(values ...string) error {
	for _, v := range values {
		if strings.ContainsAny(v, "\r\n\x00") {
			return errors.New("bounce: header value contains forbidden control character")
		}
	}
	return nil
}

func writeHeaderLine(out *strings.Builder, name, value string) {
	if value == "" {
		return
	}
	out.WriteString(name)
	out.WriteString(": ")
	out.WriteString(value)
	out.WriteString("\r\n")
}
