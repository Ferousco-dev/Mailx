package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/dkim"
	maildomain "github.com/Ferousco-dev/mailx/internal/domain"
	"github.com/Ferousco-dev/mailx/internal/emailtemplate"
	"github.com/Ferousco-dev/mailx/internal/idempotency"
	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/outbound"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

// v0.20 HTTP idempotency tuning. Retention matches the common (e.g.
// Stripe) convention of guaranteeing replay for roughly a day; staleness
// is how long an unfinished claim is trusted before a retry is allowed to
// reclaim it (crash recovery — see database.ClaimIdempotencyKey's doc);
// poll bounds how long a request waits for a GENUINELY CONCURRENT
// duplicate to finish before giving up with 409 rather than blocking
// indefinitely.
const (
	idempotencyRetention    = 24 * time.Hour
	idempotencyStaleAfter   = 30 * time.Second
	idempotencyPollInterval = 100 * time.Millisecond
	idempotencyPollTimeout  = 5 * time.Second
)

// emailHandler holds every dependency POST/GET/LIST need. It depends on
// concrete *database.DB/*storage.FileStore (not narrow interfaces): there
// is exactly one implementation of each in this codebase and no test seam
// has needed one yet — an interface here would be speculative.
type emailHandler struct {
	db    *database.DB
	store *storage.FileStore
	// dkim signs accepted messages; nil disables signing (tests only: NewServer
	// requires it).
	dkim *dkim.Service
	now  func() time.Time
	// abuse holds outbound abuse controls; nil disables them.
	abuse *AbuseControls
	// msgDomain is the right-hand side of generated Message-IDs (RFC 5322 3.6.4:
	// a domain of the generating host). It is MailX's infrastructure hostname,
	// never a tenant domain, and defaults to the local development identity.
	msgDomain string
}

// recipientLimit is the per-message recipient cap: the abuse policy's value when
// configured, otherwise the built-in default.
func (h *emailHandler) recipientLimit() int {
	if h.abuse != nil && h.abuse.Policy.MaxRecipientsPerMessage > 0 {
		return h.abuse.Policy.MaxRecipientsPerMessage
	}
	return defaultMaxRecipients
}

// renderTemplate loads templateID (tenant-scoped) and substitutes variables.
// Rendering happens here, once, before outbound.Build/DKIM — never again at
// delivery or retry time (see docs/design-v0.33.md).
func (h *emailHandler) renderTemplate(ctx context.Context, tenantID, templateID string, variables map[string]string) (emailtemplate.Rendered, *apiError) {
	t, err := h.db.GetTemplate(ctx, tenantID, templateID)
	if errors.Is(err, database.ErrNotFound) {
		return emailtemplate.Rendered{}, newError(ErrNotFoundType, "template_not_found", "no template found with that id")
	}
	if err != nil {
		return emailtemplate.Rendered{}, newError(ErrInternal, "internal_error", "failed to load template")
	}
	rendered, err := emailtemplate.Render(t.Subject, t.Text, t.HTML, variables)
	if err != nil {
		return emailtemplate.Rendered{}, newError(ErrValidation, "template_render_failed", "rendered content exceeds the maximum size")
	}
	return rendered, nil
}

const defaultMessageIDDomain = "mailx.local"

// messageID is the ONE place a Message-ID is built, used for both the message
// header and the stored metadata so they can never differ. It is generated before
// DKIM signing, so the signature covers it and no stored byte changes afterwards.
func (h *emailHandler) messageID(id string) string {
	return "<" + id + "@" + h.msgDomain + ">"
}

func newEmailHandler(db *database.DB, store *storage.FileStore) *emailHandler {
	return &emailHandler{db: db, store: store, now: func() time.Time { return time.Now().UTC() }, msgDomain: defaultMessageIDDomain}
}

// handleSend implements POST /v1/emails. See the v0.18 report's "Acceptance
// contract" section for exactly what the 202 this returns does and does
// not mean; in short, it means the message is durably recorded and MailX
// has durable responsibility for eventually attempting it — nothing about
// SMTP delivery, inbox placement, or Redis having received it yet.
func (h *emailHandler) handleSend(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}

	// Validated up front, cheaply, before touching the body: an
	// idempotency key is entirely optional (see the v0.20 report's
	// "required vs optional" decision), but if one is supplied it must be
	// well-formed before any other work happens.
	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey != "" {
		if err := idempotency.ValidateKey(idemKey); err != nil {
			writeError(w, r, newError(ErrInvalidRequest, "invalid_idempotency_key", err.Error()))
			return
		}
	}

	var req sendEmailRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_json", "request body is not valid JSON: "+err.Error()))
		return
	}

	now := h.now()
	scheduledAt, verr := req.validate(now, h.recipientLimit())
	if verr != nil {
		writeError(w, r, verr)
		return
	}

	tenantID := tenantFromContext(r.Context())
	resp, replayed, aerr := h.acceptOne(r.Context(), tenantID, req, idemKey, now, scheduledAt)
	if aerr != nil {
		writeError(w, r, aerr)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// acceptOne runs the entire per-message acceptance pipeline that used to
// live inline in handleSend — template render, build, From-domain
// authorization + DKIM signing, recipient suppression check, idempotency
// claim, abuse controls, FileStore write, and the durable InsertMessage —
// and is now shared by handleSend (one HTTP request, one email) and
// handleSendBatch (one HTTP request, N independent emails, v0.41). It
// never writes to an http.ResponseWriter: every failure is returned as an
// *apiError so a batch caller can attribute it to the right item instead
// of it corrupting a shared response. Every ordering guarantee documented
// inline below (idempotency claim after validation, abuse controls after
// the claim, FileStore before the DB transaction, etc.) is unchanged from
// the pre-v0.41 handleSend and applies per item, independently.
func (h *emailHandler) acceptOne(ctx context.Context, tenantID string, req sendEmailRequest, idemKey string, now, scheduledAt time.Time) (resp email, replayed bool, apiErr *apiError) {
	id, err := storage.NewID()
	if err != nil {
		return email{}, false, newError(ErrInternal, "internal_error", "failed to generate message id")
	}

	// PR review fix: an already-COMPLETED replay is checked here, BEFORE
	// template lookup/rendering below. Previously a retry with the same
	// Idempotency-Key and an unchanged fingerprint could still fail with
	// template_not_found/a render error if the template was edited or
	// deleted after the original request was accepted — contradicting the
	// documented replay contract (the fingerprint is on req, never on
	// rendered output, precisely so template mutation can't affect a
	// replay). This is a cheap read-only check, not a claim: if nothing is
	// completed yet, request processing continues exactly as before and the
	// existing late resolveIdempotency call still claims/polls as usual.
	if idemKey != "" {
		if fingerprint, ferr := idempotency.Fingerprint(req); ferr == nil {
			if r, ok := h.replayIfCompleted(ctx, tenantID, idempotency.OperationEmailsCreate, idemKey, fingerprint); ok {
				return r, true, nil
			}
		}
	}

	subject, text, html := req.Subject, req.Text, req.HTML
	if req.TemplateID != "" {
		rendered, terr := h.renderTemplate(ctx, tenantID, req.TemplateID, req.Variables)
		if terr != nil {
			return email{}, false, terr
		}
		subject, text, html = rendered.Subject, rendered.Text, rendered.HTML
	}

	// req itself (never subject/text/html above) is what idempotency.Fingerprint
	// hashes below: it always carries template_id+variables, never rendered
	// output, so editing a template between an accepted request and a retry
	// with the same Idempotency-Key does not change the fingerprint — the retry
	// correctly replays the ORIGINAL accepted (already-rendered) message rather
	// than re-rendering or conflicting.
	built, err := outbound.Build(outbound.Request{
		From: req.From, To: req.To, Cc: req.Cc, Bcc: req.Bcc, ReplyTo: req.ReplyTo,
		Subject: subject, Text: text, HTML: html,
		MessageID: h.messageID(id), Date: now,
	})
	if err != nil {
		return email{}, false, newError(ErrValidation, "invalid_message", err.Error())
	}
	if !sameDeliveryDomain(built.Envelope) {
		return email{}, false, newError(ErrValidation, "mixed_recipient_domains", "all recipients must share one domain for this MailX version")
	}

	fromDomain, raw, aerr := h.authorizeAndSign(ctx, tenantID, built.From, built.Raw)
	if aerr != nil {
		return email{}, false, aerr
	}
	deliverable, aerr := h.checkRecipientsForAcceptance(ctx, tenantID, built.Envelope)
	if aerr != nil {
		return email{}, false, aerr
	}

	parsed, err := mail.ParseMessage(raw)
	if err != nil {
		// Build produced something MailX's own parser rejects — a bug in
		// the builder, never a client input problem.
		return email{}, false, newError(ErrInternal, "internal_error", "failed to finalize message")
	}

	// Idempotency claim happens HERE — after every validation step above
	// has already succeeded, so a malformed/invalid request never
	// consumes the key (a corrected retry with the same key can still
	// proceed normally), but BEFORE the FileStore write, so a request
	// that loses the ownership race (a genuine concurrent duplicate)
	// never does that work at all. See internal/database.
	// ClaimIdempotencyKey's doc for the concurrency design; PostgreSQL,
	// not this handler, is what actually arbitrates concurrent claims.
	var idemCompletion *database.IdempotencyCompletion
	var idemClaimedAt time.Time
	if idemKey != "" {
		fingerprint, ferr := idempotency.Fingerprint(req)
		if ferr != nil {
			return email{}, false, newError(ErrInternal, "internal_error", "failed to fingerprint request")
		}
		r, handled, claimedAt, herr := h.resolveIdempotency(ctx, tenantID, idemKey, fingerprint, now)
		if herr != nil {
			return email{}, false, herr
		}
		if handled {
			return r, true, nil
		}
		idemClaimedAt = claimedAt
		idemCompletion = &database.IdempotencyCompletion{Operation: idempotency.OperationEmailsCreate, IdempotencyKey: idemKey, Fingerprint: fingerprint}
	}

	// Abuse controls run only for a request that will really create a message:
	// replays (handled above) and 409s never reach here, so they are never
	// charged. A refusal releases the idempotency claim it owns.
	if aerr := h.admitSend(ctx, tenantID, deliverable); aerr != nil {
		h.releaseClaim(tenantID, idemCompletion, idemClaimedAt)
		return email{}, false, aerr
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
		return email{}, false, newError(ErrInternal, "internal_error", "failed to persist message")
	}

	recipients := make([]database.RecipientInput, 0, len(built.To)+len(built.Cc)+len(built.Bcc))
	recipients = appendRoleRecipients(recipients, built.To, "to")
	recipients = appendRoleRecipients(recipients, built.Cc, "cc")
	recipients = appendRoleRecipients(recipients, built.Bcc, "bcc")

	msg, err := h.db.InsertMessage(ctx, database.NewMessage{
		ID: id, TenantID: tenantID, MailFrom: built.From, FromHeader: req.From,
		Subject: subject, MessageIDHeader: h.messageID(id),
		Recipients: recipients, AvailableAt: scheduledAt, IdempotencyCompletion: idemCompletion,
		SenderDomain: fromDomain,
	})
	if errors.Is(err, database.ErrSenderNotAuthorized) { // domain removed after the pre-check
		return email{}, false, newError(ErrForbidden, "from_domain_not_authorized", "the From domain is not a verified domain of this account")
	}
	if err != nil {
		// Lost the completion race for our OWN idempotency claim: someone
		// else (a reclaimer after we stalled past the staleness window)
		// finished first. If they were retrying the exact same payload as
		// us (same fingerprint), that is not really a conflict — it is
		// exactly the "genuinely concurrent identical retry" case this
		// feature exists for, so replay THEIR result instead of failing
		// a request that would otherwise have succeeded.
		if idemCompletion != nil && errors.Is(err, database.ErrConflict) {
			if r, ok := h.replayIfCompleted(ctx, tenantID, idemCompletion.Operation, idemCompletion.IdempotencyKey, idemCompletion.Fingerprint); ok {
				return r, true, nil
			}
		}
		// The outbox insert is part of the SAME transaction as the
		// message/recipients: a failure here durably rolls back
		// everything, so the "no orphaned DB row" guarantee holds even
		// though outbox is a separate table.
		return email{}, false, newError(ErrInternal, "internal_error", "failed to durably record message")
	}

	return emailFromRow(msg, recipientsFromInputs(id, recipients)), false, nil
}

// resolveIdempotency claims (tenantID, operation, idemKey) or discovers
// who already holds it. handled=true means the caller must respond
// immediately with resp (a replay) or err (a conflict/timeout) and must
// NOT proceed to create a message; handled=false means the caller now
// owns the claim and must complete it via IdempotencyCompletion.
func (h *emailHandler) resolveIdempotency(ctx context.Context, tenantID, idemKey, fingerprint string, now time.Time) (resp email, handled bool, claimedAt time.Time, err *apiError) {
	claim, owned, dbErr := h.db.ClaimIdempotencyKey(ctx, tenantID, idempotency.OperationEmailsCreate, idemKey, fingerprint,
		now.Add(idempotencyRetention), now.Add(-idempotencyStaleAfter))
	if dbErr != nil {
		return email{}, false, time.Time{}, newError(ErrInternal, "internal_error", "failed to process idempotency key")
	}
	if owned {
		return email{}, false, claim.CreatedAt, nil
	}

	if claim.Fingerprint != fingerprint {
		// Never echo the original request back — only confirm reuse
		// happened, not what the original payload contained.
		return email{}, true, time.Time{}, newError(ErrConflictType, "idempotency_key_conflict",
			"this Idempotency-Key was already used with a different request")
	}

	if claim.Status == database.IdempotencyInProgress {
		var perr *apiError
		claim, perr = h.pollIdempotencyCompletion(ctx, tenantID, idemKey)
		if perr != nil {
			return email{}, true, time.Time{}, perr
		}
	}

	// claim.Status == completed here (poll only returns on completion or
	// a timeout error above) — replay the CURRENT resource state (see
	// the v0.20 report's "resource evolution" decision: a replay reflects
	// where the resource is now, e.g. delivered instead of queued, not a
	// frozen snapshot of the original 202 body; only the id and the 202
	// status code itself are guaranteed stable across replays).
	resp, ok := h.loadReplay(ctx, tenantID, claim)
	if !ok {
		return email{}, true, time.Time{}, newError(ErrInternal, "internal_error", "failed to load the original result for this idempotency key")
	}
	return resp, true, time.Time{}, nil
}

// replayIfCompleted is the recovery path for a caller that lost the
// completion race for its OWN claim (see handleSend's InsertMessage error
// handling): if the current claim holder finished with the SAME
// fingerprint, this was a genuinely concurrent identical retry, not a
// real conflict, so its result is replayed rather than surfacing an error
// for a request that would otherwise have succeeded.
func (h *emailHandler) replayIfCompleted(ctx context.Context, tenantID, operation, idemKey, fingerprint string) (email, bool) {
	claim, err := h.db.GetIdempotencyKey(ctx, tenantID, operation, idemKey)
	if err != nil || claim.Status != database.IdempotencyCompleted || claim.Fingerprint != fingerprint {
		return email{}, false
	}
	return h.loadReplay(ctx, tenantID, claim)
}

func (h *emailHandler) loadReplay(ctx context.Context, tenantID string, claim database.IdempotencyRecord) (email, bool) {
	if claim.ResourceID == nil {
		return email{}, false
	}
	msg, err := h.db.GetMessage(ctx, tenantID, *claim.ResourceID)
	if err != nil {
		return email{}, false
	}
	recipients, err := h.db.ListRecipients(ctx, msg.ID)
	if err != nil {
		return email{}, false
	}
	return emailFromRow(msg, recipients), true
}

// pollIdempotencyCompletion waits, bounded, for a genuinely concurrent
// duplicate request to finish — never indefinitely, and it stops early if
// ctx is canceled (the client gave up, so this request should too).
func (h *emailHandler) pollIdempotencyCompletion(ctx context.Context, tenantID, idemKey string) (database.IdempotencyRecord, *apiError) {
	deadline := h.now().Add(idempotencyPollTimeout)
	ticker := time.NewTicker(idempotencyPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return database.IdempotencyRecord{}, newError(ErrTemporarilyUnavailable, "idempotency_in_progress",
				"a request with this Idempotency-Key is still being processed")
		case <-ticker.C:
			claim, err := h.db.GetIdempotencyKey(ctx, tenantID, idempotency.OperationEmailsCreate, idemKey)
			if err != nil {
				return database.IdempotencyRecord{}, newError(ErrInternal, "internal_error", "failed to check idempotency key status")
			}
			if claim.Status == database.IdempotencyCompleted {
				return claim, nil
			}
			if h.now().After(deadline) {
				return database.IdempotencyRecord{}, newError(ErrConflictType, "idempotency_in_progress",
					"a request with this Idempotency-Key is still being processed; retry shortly")
			}
		}
	}
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

// authorizeAndSign enforces verified-From and signs the final message bytes. It
// writes the error response itself and reports ok=false when the request must
// stop. Verified-From (v0.26): the RFC 5322 From domain must be a domain this
// tenant owns and has verified, EXACTLY (no parent/subdomain inference; see
// internal/domain). It runs before the idempotency claim, the FileStore write
// and every durable insert, so an unauthorized sender is never accepted
// responsibility for. The SMTP MAIL FROM is the same address today (envelope
// sender), so both identities are authorized; bounce/return-path architecture is
// unchanged. Signing happens at the point the message bytes become final: what
// is signed here is exactly what is stored, queued and transmitted. A domain
// with an active DKIM key must sign; any key failure refuses the message rather
// than sending it unsigned. No key means DKIM is not set up for the domain.
func (h *emailHandler) authorizeAndSign(ctx context.Context, tenantID, envelopeFrom, raw string) (fromDomain, signed string, apiErr *apiError) {
	fromDomain, err := maildomain.FromDomain(envelopeFrom)
	if err != nil {
		return "", "", newError(ErrValidation, "invalid_from", "the From address does not have a valid domain")
	}
	if _, err := h.db.VerifiedSenderDomain(ctx, tenantID, fromDomain); err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return "", "", newError(ErrForbidden, "from_domain_not_authorized", "the From domain is not a verified domain of this account")
		}
		return "", "", newError(ErrInternal, "internal_error", "failed to authorize sender")
	}
	if h.dkim == nil {
		return fromDomain, raw, nil
	}
	out, _, serr := h.dkim.SignMessage(ctx, tenantID, fromDomain, []byte(raw))
	if serr != nil {
		return "", "", newError(ErrTemporarilyUnavailable, "dkim_signing_unavailable", "message signing is unavailable for this domain right now; retry later")
	}
	return fromDomain, string(out), nil
}
