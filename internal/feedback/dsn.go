package feedback

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"
)

// ErrNotDSN means the input is not a multipart/report delivery-status message
// MailX can classify — never a caller's own message text, never trusted for
// classification.
var ErrNotDSN = errors.New("feedback: not a recognizable delivery-status report")

// ErrOversized means the raw input exceeded MaxInputBytes; ParseDSN never reads
// past that bound.
var ErrOversized = errors.New("feedback: input exceeds the maximum feedback size")

// ParseDSN parses an RFC 3461/3462/3464 multipart/report delivery-status
// message and returns one ParsedDSN per per-recipient field group (RFC 3464
// 3.2). It is a pure structural parser: it trusts nothing about WHO sent this
// (that is the ingestion boundary's job — see internal/api) and extracts only
// the fields RFC 3464 defines, never the human-readable part.
//
// Bounded and panic-safe by construction: len(raw) is checked up front
// (ErrOversized), multipart.Reader.NextPart enforces MIME structure without
// unbounded recursion (multipart/report has no legal nested multipart/report),
// and every part is read through an io.LimitReader so a part that lies about
// its own length cannot force unbounded allocation. A malformed part is
// skipped (tolerant of unknown/broken structure elsewhere in the report) rather
// than aborting the whole message, EXCEPT when no delivery-status part is found
// at all, which is ErrNotDSN.
func ParseDSN(raw []byte) ([]ParsedDSN, error) {
	if len(raw) == 0 {
		return nil, ErrNotDSN
	}
	if len(raw) > MaxInputBytes {
		return nil, ErrOversized
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotDSN, err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "multipart/report") {
		return nil, ErrNotDSN
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, ErrNotDSN
	}

	mr := multipart.NewReader(msg.Body, boundary)
	var out []ParsedDSN
	found := false
	for i := 0; i < maxMIMEParts; i++ {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break // malformed trailing structure: keep whatever we already parsed
		}
		partType, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if !strings.EqualFold(partType, "message/delivery-status") {
			_, _ = io.CopyN(io.Discard, part, MaxInputBytes) // drain, bounded, so NextPart can proceed
			continue
		}
		found = true
		groups, perr := parseDeliveryStatus(part)
		if perr != nil {
			continue // this part was malformed; a later message/delivery-status part (rare) still gets a chance
		}
		out = append(out, groups...)
	}
	if !found {
		return nil, ErrNotDSN
	}
	return out, nil
}

// parseDeliveryStatus reads RFC 3464 3.1/3.2: one per-message field group
// (fields we do not need beyond skipping it), followed by one or more
// per-recipient field groups, each block terminated by a blank line — exactly
// the shape net/textproto's MIME-header reader already parses safely and
// bounded (textproto.Reader itself caps line/header size).
func parseDeliveryStatus(r io.Reader) ([]ParsedDSN, error) {
	tp := textproto.NewReader(bufio.NewReader(io.LimitReader(r, MaxInputBytes)))

	// Per-message fields (Reporting-MTA, Original-Envelope-Id, ...): MailX only
	// needs Reporting-MTA per recipient group below, so this first block is read
	// and discarded, but reading it is required to advance past it to the
	// per-recipient groups that follow.
	msgFields, err := tp.ReadMIMEHeader()
	if err != nil && len(msgFields) == 0 {
		return nil, err
	}
	reportingMTA := lastMTAValue(msgFields.Get("Reporting-Mta"))

	var out []ParsedDSN
	for i := 0; i < maxMIMEParts; i++ {
		fields, err := tp.ReadMIMEHeader()
		if len(fields) == 0 {
			break // EOF or a blank remainder: no more recipient groups
		}
		out = append(out, ParsedDSN{
			Action:            parseAction(strings.ToLower(strings.TrimSpace(fields.Get("Action")))),
			Status:            strings.TrimSpace(fields.Get("Status")),
			FinalRecipient:    addressValue(fields.Get("Final-Recipient")),
			OriginalRecipient: addressValue(fields.Get("Original-Recipient")),
			DiagnosticCode:    sanitize(lastMTAValue(fields.Get("Diagnostic-Code")), maxDiagnosticLen),
			RemoteMTA:         sanitize(lastMTAValue(fields.Get("Remote-Mta")), maxMTALen),
			ReportingMTA:      sanitize(reportingMTA, maxMTALen),
		})
		if err != nil {
			break
		}
	}
	if len(out) == 0 {
		return nil, ErrNotDSN
	}
	return out, nil
}

// addressValue strips an RFC 3464 "type;address" field down to the address,
// e.g. "rfc822;bob@example.com" -> "bob@example.com". A field with no ";" or an
// empty value returns "" (caller treats that recipient group as unmatched
// rather than guessing).
func addressValue(field string) string {
	field = strings.TrimSpace(field)
	i := strings.IndexByte(field, ';')
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(field[i+1:])
}

// lastMTAValue strips a "type;value" prefix the same way addressValue does,
// but tolerates a bare value (some senders omit the type for Diagnostic-Code
// and *-MTA fields even though RFC 3464 requires it) by returning it unchanged
// when there is no ";".
func lastMTAValue(field string) string {
	field = strings.TrimSpace(field)
	if i := strings.IndexByte(field, ';'); i >= 0 {
		return strings.TrimSpace(field[i+1:])
	}
	return field
}

// sanitize bounds length and strips control characters (defense against log
// injection and header injection if this value is ever echoed — see
// docs/design-v0.32.md "Security"): only printable ASCII/UTF-8 survives, CR/LF
// and other control bytes become a space.
func sanitize(s string, max int) string {
	if len(s) > max {
		s = s[:max]
	}
	b := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\r' || r == '\n' || r < 0x20 || r == 0x7f {
			b = append(b, ' ')
			continue
		}
		b = append(b, r)
	}
	return strings.TrimSpace(string(b))
}
