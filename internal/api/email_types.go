package api

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
)

const (
	defaultMaxRecipients = 50
	maxSubjectLen        = 500
	maxBodyLen           = 2 << 20 // 2 MiB per body part
	defaultLimit         = 20
	maxLimit             = 100
)

// sendEmailRequest is POST /v1/emails' exact wire schema. Unknown JSON
// fields are rejected (see handler's DisallowUnknownFields) rather than
// silently ignored — a client relying on a field MailX doesn't implement
// (attachments, tags, custom headers: all explicitly deferred, see the
// v0.18 report) gets a clear 422, not a silently-dropped feature.
type sendEmailRequest struct {
	From        string   `json:"from"`
	To          []string `json:"to"`
	Cc          []string `json:"cc"`
	Bcc         []string `json:"bcc"`
	ReplyTo     string   `json:"reply_to"`
	Subject     string   `json:"subject"`
	HTML        string   `json:"html"`
	Text        string   `json:"text"`
	ScheduledAt *string  `json:"scheduled_at"` // RFC 3339; nil/absent = send now
}

func (req sendEmailRequest) validate(now time.Time, maxRecipients int) (scheduledAt time.Time, err *apiError) {
	if strings.TrimSpace(req.From) == "" {
		return time.Time{}, newError(ErrValidation, "missing_from", "from is required")
	}
	if len(req.To) == 0 {
		return time.Time{}, newError(ErrValidation, "missing_recipient", "at least one 'to' recipient is required")
	}
	total := len(req.To) + len(req.Cc) + len(req.Bcc)
	if total > maxRecipients {
		return time.Time{}, newError(ErrValidation, "too_many_recipients", fmt.Sprintf("a message may address at most %d recipients across to/cc/bcc, got %d", maxRecipients, total))
	}
	if len(req.Subject) > maxSubjectLen {
		return time.Time{}, newError(ErrValidation, "subject_too_long", fmt.Sprintf("subject must be at most %d characters", maxSubjectLen))
	}
	if len(req.HTML) > maxBodyLen || len(req.Text) > maxBodyLen {
		return time.Time{}, newError(ErrValidation, "body_too_large", fmt.Sprintf("html/text body must each be at most %d bytes", maxBodyLen))
	}
	if strings.TrimSpace(req.HTML) == "" && strings.TrimSpace(req.Text) == "" {
		return time.Time{}, newError(ErrValidation, "missing_body", "at least one of html or text is required")
	}
	if req.ScheduledAt == nil {
		return time.Time{}, nil
	}
	t, parseErr := time.Parse(time.RFC3339, *req.ScheduledAt)
	if parseErr != nil {
		return time.Time{}, newError(ErrValidation, "invalid_scheduled_at", "scheduled_at must be an RFC 3339 timestamp")
	}
	if t.Before(now) {
		return time.Time{}, newError(ErrValidation, "invalid_scheduled_at", "scheduled_at must not be in the past")
	}
	return t, nil
}

// email is the public resource. html/text are populated only by GET
// /v1/emails/{id} — the list endpoint omits both (see report's
// "Public Email schema" section) to keep listing cheap, since answering
// it would otherwise require re-reading and re-parsing raw MIME from
// FileStore for every row.
type email struct {
	ID          string     `json:"id"`
	From        string     `json:"from"`
	To          []string   `json:"to"`
	Cc          []string   `json:"cc,omitempty"`
	Bcc         []string   `json:"bcc,omitempty"`
	ReplyTo     string     `json:"reply_to,omitempty"`
	Subject     string     `json:"subject"`
	HTML        *string    `json:"html,omitempty"`
	Text        *string    `json:"text,omitempty"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	QueuedAt    *time.Time `json:"queued_at,omitempty"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}

// emailFromRow builds the public resource from a database.Message plus its
// recipients; header_kind (the role explicitly submitted at send time)
// sorts each address into to/cc/bcc — an address with no header_kind (a
// hidden envelope-only recipient from the legacy SMTP path) is omitted
// here, since the public API has no role to report it under.
func emailFromRow(msg database.Message, recipients []database.Recipient) email {
	e := email{
		ID:          msg.ID,
		From:        msg.FromHeader,
		Subject:     msg.Subject,
		Status:      string(msg.Status),
		CreatedAt:   msg.CreatedAt,
		QueuedAt:    msg.QueuedAt,
		DeliveredAt: msg.DeliveredAt,
	}
	if e.From == "" {
		e.From = msg.MailFrom
	}
	for _, r := range recipients {
		if r.HeaderKind == nil {
			continue
		}
		addr := strings.TrimPrefix(strings.TrimSuffix(r.Address, ">"), "<")
		switch *r.HeaderKind {
		case "to":
			e.To = append(e.To, addr)
		case "cc":
			e.Cc = append(e.Cc, addr)
		case "bcc":
			e.Bcc = append(e.Bcc, addr)
		}
	}
	return e
}

type emailList struct {
	Data       []email `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

// encodeCursor/decodeCursor make database.MessageCursor opaque to
// clients: they must treat it as a token, not parse it. Base64 (not
// encryption/signing) is enough here since the cursor only reveals a
// timestamp+id already visible in the very same response's rows.
func encodeCursor(c database.MessageCursor) string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(token string) (database.MessageCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return database.MessageCursor{}, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return database.MessageCursor{}, fmt.Errorf("malformed cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return database.MessageCursor{}, err
	}
	return database.MessageCursor{CreatedAt: t, ID: parts[1]}, nil
}
