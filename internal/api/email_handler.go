package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/outbound"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

// emailHandler holds every dependency POST/GET/LIST need. It depends on
// concrete *database.DB/*storage.FileStore (not narrow interfaces): there
// is exactly one implementation of each in this codebase and no test seam
// has needed one yet — an interface here would be speculative.
type emailHandler struct {
	db    *database.DB
	store *storage.FileStore
	now   func() time.Time
}

func newEmailHandler(db *database.DB, store *storage.FileStore) *emailHandler {
	return &emailHandler{db: db, store: store, now: func() time.Time { return time.Now().UTC() }}
}

// handleSend implements POST /v1/emails. See the v0.18 report's "Acceptance
// contract" section for exactly what the 202 this returns does and does
// not mean; in short, it means the message is durably recorded and MailX
// has durable responsibility for eventually attempting it — nothing about
// SMTP delivery, inbox placement, or Redis having received it yet.
func (h *emailHandler) handleSend(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && ct != "application/json" {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}

	var req sendEmailRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_json", "request body is not valid JSON: "+err.Error()))
		return
	}

	now := h.now()
	scheduledAt, verr := req.validate(now)
	if verr != nil {
		writeError(w, r, verr)
		return
	}

	id, err := storage.NewID()
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to generate message id"))
		return
	}

	built, err := outbound.Build(outbound.Request{
		From: req.From, To: req.To, Cc: req.Cc, Bcc: req.Bcc, ReplyTo: req.ReplyTo,
		Subject: req.Subject, Text: req.Text, HTML: req.HTML,
		MessageID: fmt.Sprintf("<%s@mailx.local>", id), Date: now,
	})
	if err != nil {
		writeError(w, r, newError(ErrValidation, "invalid_message", err.Error()))
		return
	}
	if !sameDeliveryDomain(built.Envelope) {
		writeError(w, r, newError(ErrValidation, "mixed_recipient_domains", "all recipients must share one domain for this MailX version"))
		return
	}

	parsed, err := mail.ParseMessage(built.Raw)
	if err != nil {
		// Build produced something MailX's own parser rejects — a bug in
		// the builder, never a client input problem.
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to finalize message"))
		return
	}

	// FileStore write happens BEFORE the durable DB transaction: if this
	// fails, nothing durable references `id` yet (safe to just error out).
	// If it succeeds but the DB transaction below fails, `id` is an
	// orphaned-but-harmless file — nothing ever points a client or a
	// worker at it. The reverse (a DB row with no backing file) can never
	// happen, which is the property that matters: the API never accepts a
	// message it cannot actually construct into deliverable bytes.
	record := storage.MessageRecord{
		ID: id, ReceivedAt: now,
		Envelope: mail.Envelope{MailFrom: built.From, Recipients: built.Envelope},
		Message:  parsed,
	}
	if err := h.store.Save(record); err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to persist message"))
		return
	}

	recipients := make([]database.RecipientInput, 0, len(built.To)+len(built.Cc)+len(built.Bcc))
	recipients = appendRoleRecipients(recipients, built.To, "to")
	recipients = appendRoleRecipients(recipients, built.Cc, "cc")
	recipients = appendRoleRecipients(recipients, built.Bcc, "bcc")

	tenantID := tenantFromContext(r.Context())
	msg, err := h.db.InsertMessage(r.Context(), database.NewMessage{
		ID: id, TenantID: tenantID, MailFrom: built.From, FromHeader: req.From,
		Subject: req.Subject, MessageIDHeader: fmt.Sprintf("<%s@mailx.local>", id),
		Recipients: recipients, AvailableAt: scheduledAt,
	})
	if err != nil {
		// The outbox insert is part of the SAME transaction as the
		// message/recipients: a failure here durably rolls back
		// everything, so the "no orphaned DB row" guarantee holds even
		// though outbox is a separate table.
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to durably record message"))
		return
	}

	writeJSON(w, http.StatusAccepted, emailFromRow(msg, recipientsFromInputs(id, recipients)))
}

// The current queue schedules one delivery request per message, and the
// delivery engine requires every envelope recipient to match its domain.
// Reject unsupported mixed-domain sends before persistence or acceptance.
func sameDeliveryDomain(recipients []string) bool {
	if len(recipients) == 0 {
		return false
	}
	first := recipientDeliveryDomain(recipients[0])
	for _, recipient := range recipients[1:] {
		if !strings.EqualFold(first, recipientDeliveryDomain(recipient)) {
			return false
		}
	}
	return true
}

func recipientDeliveryDomain(recipient string) string {
	at := strings.LastIndexByte(recipient, '@')
	if at < 0 {
		return ""
	}
	return strings.TrimSuffix(recipient[at+1:], ">")
}

func appendRoleRecipients(out []database.RecipientInput, addrs []string, role string) []database.RecipientInput {
	r := role
	for _, a := range addrs {
		out = append(out, database.RecipientInput{Address: a, HeaderKind: &r})
	}
	return out
}

// recipientsFromInputs lets handleSend build its response from the exact
// recipients it just sent to InsertMessage, without a second DB round
// trip just to re-read what this same request already knows.
func recipientsFromInputs(messageID string, in []database.RecipientInput) []database.Recipient {
	out := make([]database.Recipient, 0, len(in))
	for _, r := range in {
		out = append(out, database.Recipient{MessageID: messageID, Address: r.Address, HeaderKind: r.HeaderKind})
	}
	return out
}

// handleGet implements GET /v1/emails/{id}: tenant-scoped lookup, full
// resource including html/text (re-derived from FileStore's raw MIME —
// see email.go's doc on why the list endpoint does not do this).
func (h *emailHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tenantID := tenantFromContext(r.Context())

	msg, err := h.db.GetMessage(r.Context(), tenantID, id)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			writeError(w, r, newError(ErrNotFoundType, "email_not_found", "no email found with that id"))
			return
		}
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load email"))
		return
	}
	recipients, err := h.db.ListRecipients(r.Context(), msg.ID)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load recipients"))
		return
	}

	resource := emailFromRow(msg, recipients)
	if loaded, err := h.store.Load(msg.ID); err == nil {
		if parsed, err := mail.ParseMessage(string(loaded.Raw)); err == nil {
			if parsed.TextBody != "" {
				resource.Text = &parsed.TextBody
			}
			if parsed.HTMLBody != "" {
				resource.HTML = &parsed.HTMLBody
			}
		}
	}
	writeJSON(w, http.StatusOK, resource)
}

// handleList implements GET /v1/emails: tenant-scoped, cursor-paginated,
// optionally filtered by status. See email_types.go for cursor encoding.
func (h *emailHandler) handleList(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	q := r.URL.Query()

	limit := defaultLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, r, newError(ErrValidation, "invalid_limit", fmt.Sprintf("limit must be an integer between 1 and %d", maxLimit)))
			return
		}
		limit = n
	}

	var after *database.MessageCursor
	if raw := q.Get("cursor"); raw != "" {
		c, err := decodeCursor(raw)
		if err != nil {
			writeError(w, r, newError(ErrInvalidRequest, "invalid_cursor", "cursor is not valid"))
			return
		}
		after = &c
	}

	var status *database.MessageStatus
	if raw := q.Get("status"); raw != "" {
		s := database.MessageStatus(raw)
		if !s.Valid() {
			writeError(w, r, newError(ErrValidation, "invalid_status", "unrecognized status filter"))
			return
		}
		status = &s
	}

	rows, err := h.db.ListMessages(r.Context(), tenantID, status, limit, after)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to list emails"))
		return
	}

	resp := emailList{Data: make([]email, 0, len(rows))}
	for _, msg := range rows {
		resp.Data = append(resp.Data, emailFromRow(msg, nil))
	}
	if len(rows) == limit {
		last := rows[len(rows)-1]
		cursor := encodeCursor(database.MessageCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		resp.NextCursor = &cursor
	}
	writeJSON(w, http.StatusOK, resp)
}
