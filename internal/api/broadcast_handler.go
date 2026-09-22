package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/domain"
	"github.com/Ferousco-dev/mailx/internal/emailtemplate"
	"github.com/Ferousco-dev/mailx/internal/idempotency"
)

// broadcastHandler accepts bulk-send requests. It durably records the
// broadcast and returns 202 quickly; it NEVER expands the audience, renders,
// signs, or sends anything itself — internal/broadcast.Expander does that
// asynchronously and in bounded steps. See docs/design-v0.36.md.
type broadcastHandler struct {
	db  *database.DB
	now func() time.Time
}

type createBroadcastRequest struct {
	Name       string            `json:"name"`
	AudienceID string            `json:"audience_id"`
	TemplateID string            `json:"template_id"`
	From       string            `json:"from"`
	ReplyTo    string            `json:"reply_to"`
	Variables  map[string]string `json:"variables"`
	// SendAt (v0.37): RFC 3339. Nil/absent means immediate — unchanged v0.36
	// behavior. Set means expansion must not start before this instant; it
	// is part of the idempotency fingerprint like every other field, so a
	// retry with the same key but a different send_at conflicts (409)
	// rather than silently rescheduling.
	SendAt *string `json:"send_at"`
}

// broadcastResource never claims delivery: status values are orchestration
// states (accepted/expanding/completed/failed), and "completed" means every
// recipient was handed to the existing send pipeline or suppressed — never
// that mail reached an inbox.
type broadcastResource struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	AudienceID string     `json:"audience_id"`
	TemplateID string     `json:"template_id"`
	From       string     `json:"from"`
	ReplyTo    string     `json:"reply_to,omitempty"`
	Status     string     `json:"status"`
	SendAt     *time.Time `json:"send_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type broadcastList struct {
	Data       []broadcastResource `json:"data"`
	NextCursor *string             `json:"next_cursor"`
}

type broadcastRecipientResource struct {
	ID        string  `json:"id"`
	ContactID string  `json:"contact_id"`
	Email     string  `json:"email"`
	Status    string  `json:"status"`
	MessageID *string `json:"message_id,omitempty"`
}

type broadcastRecipientList struct {
	Data       []broadcastRecipientResource `json:"data"`
	NextCursor *string                      `json:"next_cursor"`
}

func broadcastFromRow(b database.Broadcast) broadcastResource {
	return broadcastResource{ID: b.ID, Name: b.Name, AudienceID: b.AudienceID, TemplateID: b.TemplateID,
		From: b.FromAddress, ReplyTo: b.ReplyTo, Status: b.Status, SendAt: b.SendAt, CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt}
}

func (h *broadcastHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey != "" {
		if err := idempotency.ValidateKey(idemKey); err != nil {
			writeError(w, r, newError(ErrInvalidRequest, "invalid_idempotency_key", err.Error()))
			return
		}
	}
	var req createBroadcastRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_json", "request body is not valid JSON"))
		return
	}
	now := h.now()
	sendAt, aerr := validateBroadcastRequest(req, now)
	if aerr != nil {
		writeError(w, r, aerr)
		return
	}

	tenantID := tenantFromContext(r.Context())

	// Ownership: both resources must belong to THIS tenant, validated
	// authoritatively here (not merely because auth passed) — see
	// database.GetAudience/GetTemplate, both already tenant-scoped queries.
	// PR review fix: this validation runs BEFORE the idempotency claim
	// below (moved from after it). Claiming first meant a correctable 404/
	// 403 here left the claim stuck 'in_progress' forever (nothing released
	// it), so an immediate retry with the same key polled or got
	// idempotency_in_progress instead of a clean retry — same ordering
	// handleSend already uses for exactly this reason.
	aud, err := h.db.GetAudience(r.Context(), tenantID, req.AudienceID)
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "audience_not_found", "no audience found with that id"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load audience"))
		return
	}
	tmpl, err := h.db.GetTemplate(r.Context(), tenantID, req.TemplateID)
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "template_not_found", "no template found with that id"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load template"))
		return
	}
	fromDomain, err := domain.FromDomain(req.From)
	if err != nil {
		writeError(w, r, newError(ErrValidation, "invalid_from", "the From address does not have a valid domain"))
		return
	}
	if _, err := h.db.VerifiedSenderDomain(r.Context(), tenantID, fromDomain); err != nil {
		if errors.Is(err, database.ErrNotFound) {
			writeError(w, r, newError(ErrForbidden, "from_domain_not_authorized", "the From domain is not a verified domain of this account"))
			return
		}
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to authorize sender"))
		return
	}

	var completion *database.IdempotencyCompletion
	if idemKey != "" {
		fingerprint, ferr := idempotency.Fingerprint(req)
		if ferr != nil {
			writeError(w, r, newError(ErrInternal, "internal_error", "failed to fingerprint request"))
			return
		}
		resp, handled, herr := h.resolveIdempotency(r.Context(), tenantID, idemKey, fingerprint, now)
		if herr != nil {
			writeError(w, r, herr)
			return
		}
		if handled {
			w.Header().Set("Idempotency-Replayed", "true")
			writeJSON(w, http.StatusAccepted, resp)
			return
		}
		completion = &database.IdempotencyCompletion{Operation: idempotency.OperationBroadcastsCreate, IdempotencyKey: idemKey, Fingerprint: fingerprint}
	}

	b, err := h.db.CreateBroadcast(r.Context(), database.NewBroadcast{
		TenantID: tenantID, AudienceID: aud.ID, TemplateID: tmpl.ID, Name: req.Name,
		FromAddress: req.From, ReplyTo: req.ReplyTo,
		SubjectTemplate: tmpl.Subject, TextTemplate: tmpl.Text, HTMLTemplate: tmpl.HTML,
		Variables: req.Variables, SendAt: sendAt, IdempotencyCompletion: completion,
	})
	if err != nil {
		if completion != nil && errors.Is(err, database.ErrConflict) {
			// Lost our own completion race to a genuinely concurrent identical
			// retry — replay THEIR result (same pattern as handleSend).
			if resp, ok := h.replayIfCompleted(r.Context(), tenantID, completion.Operation, completion.IdempotencyKey, completion.Fingerprint); ok {
				w.Header().Set("Idempotency-Replayed", "true")
				writeJSON(w, http.StatusAccepted, resp)
				return
			}
		}
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to create broadcast"))
		return
	}
	writeJSON(w, http.StatusAccepted, broadcastFromRow(b))
}

func validateBroadcastRequest(req createBroadcastRequest, now time.Time) (sendAt *time.Time, err *apiError) {
	if strings.TrimSpace(req.Name) == "" {
		return nil, newError(ErrValidation, "missing_name", "name is required")
	}
	if len(req.Name) > maxAudienceNameLen {
		return nil, newError(ErrValidation, "invalid_name", fmt.Sprintf("name must be at most %d characters", maxAudienceNameLen))
	}
	if strings.TrimSpace(req.AudienceID) == "" {
		return nil, newError(ErrValidation, "missing_audience_id", "audience_id is required")
	}
	if strings.TrimSpace(req.TemplateID) == "" {
		return nil, newError(ErrValidation, "missing_template_id", "template_id is required")
	}
	if strings.TrimSpace(req.From) == "" {
		return nil, newError(ErrValidation, "missing_from", "from is required")
	}
	if err := emailtemplate.ValidateVariables(req.Variables); err != nil {
		return nil, newError(ErrValidation, "invalid_variables", err.Error())
	}
	return parseSendAt(req.SendAt, now)
}

func (h *broadcastHandler) resolveIdempotency(ctx context.Context, tenantID, idemKey, fingerprint string, now time.Time) (resp broadcastResource, handled bool, err *apiError) {
	claim, owned, dbErr := h.db.ClaimIdempotencyKey(ctx, tenantID, idempotency.OperationBroadcastsCreate, idemKey, fingerprint,
		now.Add(idempotencyRetention), now.Add(-idempotencyStaleAfter))
	if dbErr != nil {
		return broadcastResource{}, false, newError(ErrInternal, "internal_error", "failed to process idempotency key")
	}
	if owned {
		return broadcastResource{}, false, nil
	}
	if claim.Fingerprint != fingerprint {
		return broadcastResource{}, true, newError(ErrConflictType, "idempotency_key_conflict", "this Idempotency-Key was already used with a different request")
	}
	if claim.Status == database.IdempotencyInProgress {
		var perr *apiError
		claim, perr = h.pollIdempotencyCompletion(ctx, tenantID, idemKey)
		if perr != nil {
			return broadcastResource{}, true, perr
		}
	}
	resp, ok := h.loadReplay(ctx, tenantID, claim)
	if !ok {
		return broadcastResource{}, true, newError(ErrInternal, "internal_error", "failed to load the original result for this idempotency key")
	}
	return resp, true, nil
}

func (h *broadcastHandler) replayIfCompleted(ctx context.Context, tenantID, operation, idemKey, fingerprint string) (broadcastResource, bool) {
	claim, err := h.db.GetIdempotencyKey(ctx, tenantID, operation, idemKey)
	if err != nil || claim.Status != database.IdempotencyCompleted || claim.Fingerprint != fingerprint {
		return broadcastResource{}, false
	}
	return h.loadReplay(ctx, tenantID, claim)
}

func (h *broadcastHandler) loadReplay(ctx context.Context, tenantID string, claim database.IdempotencyRecord) (broadcastResource, bool) {
	if claim.ResourceID == nil {
		return broadcastResource{}, false
	}
	b, err := h.db.GetBroadcast(ctx, tenantID, *claim.ResourceID)
	if err != nil {
		return broadcastResource{}, false
	}
	return broadcastFromRow(b), true
}

func (h *broadcastHandler) pollIdempotencyCompletion(ctx context.Context, tenantID, idemKey string) (database.IdempotencyRecord, *apiError) {
	deadline := h.now().Add(idempotencyPollTimeout)
	ticker := time.NewTicker(idempotencyPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return database.IdempotencyRecord{}, newError(ErrTemporarilyUnavailable, "idempotency_in_progress", "a request with this Idempotency-Key is still being processed")
		case <-ticker.C:
			claim, err := h.db.GetIdempotencyKey(ctx, tenantID, idempotency.OperationBroadcastsCreate, idemKey)
			if err != nil {
				return database.IdempotencyRecord{}, newError(ErrInternal, "internal_error", "failed to check idempotency key status")
			}
			if claim.Status == database.IdempotencyCompleted {
				return claim, nil
			}
			if h.now().After(deadline) {
				return database.IdempotencyRecord{}, newError(ErrTemporarilyUnavailable, "idempotency_in_progress", "a request with this Idempotency-Key is still being processed")
			}
		}
	}
}

func (h *broadcastHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	b, err := h.db.GetBroadcast(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"))
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "broadcast_not_found", "no broadcast found with that id"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load broadcast"))
		return
	}
	writeJSON(w, http.StatusOK, broadcastFromRow(b))
}

func (h *broadcastHandler) handleList(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, r, newError(ErrValidation, "invalid_limit", fmt.Sprintf("limit must be an integer between 1 and %d", maxLimit)))
			return
		}
		limit = n
	}
	var after *database.BroadcastCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeBroadcastCursor(raw)
		if err != nil {
			writeError(w, r, newError(ErrInvalidRequest, "invalid_cursor", "cursor is not valid"))
			return
		}
		after = &c
	}
	rows, err := h.db.ListBroadcasts(r.Context(), tenantFromContext(r.Context()), limit+1, after)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to list broadcasts"))
		return
	}
	resp := broadcastList{Data: make([]broadcastResource, 0, min(len(rows), limit))}
	for _, row := range rows[:min(len(rows), limit)] {
		resp.Data = append(resp.Data, broadcastFromRow(row))
	}
	if len(rows) > limit {
		last := rows[limit-1]
		token := encodeBroadcastCursor(database.BroadcastCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		resp.NextCursor = &token
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleListRecipients is bounded/keyset-paginated — never the whole set.
func (h *broadcastHandler) handleListRecipients(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, r, newError(ErrValidation, "invalid_limit", fmt.Sprintf("limit must be an integer between 1 and %d", maxLimit)))
			return
		}
		limit = n
	}
	var after *database.BroadcastRecipientCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeBroadcastRecipientCursor(raw)
		if err != nil {
			writeError(w, r, newError(ErrInvalidRequest, "invalid_cursor", "cursor is not valid"))
			return
		}
		after = &c
	}
	rows, err := h.db.ListBroadcastRecipients(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"), limit+1, after)
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "broadcast_not_found", "no broadcast found with that id"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to list broadcast recipients"))
		return
	}
	resp := broadcastRecipientList{Data: make([]broadcastRecipientResource, 0, min(len(rows), limit))}
	for _, row := range rows[:min(len(rows), limit)] {
		resp.Data = append(resp.Data, broadcastRecipientResource{ID: row.ID, ContactID: row.ContactID, Email: row.Email, Status: row.Status, MessageID: row.MessageID})
	}
	if len(rows) > limit {
		last := rows[limit-1]
		token := encodeBroadcastRecipientCursor(database.BroadcastRecipientCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		resp.NextCursor = &token
	}
	writeJSON(w, http.StatusOK, resp)
}

func encodeBroadcastCursor(c database.BroadcastCursor) string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeBroadcastCursor(token string) (database.BroadcastCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return database.BroadcastCursor{}, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return database.BroadcastCursor{}, errors.New("malformed cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	return database.BroadcastCursor{CreatedAt: t, ID: parts[1]}, err
}

func encodeBroadcastRecipientCursor(c database.BroadcastRecipientCursor) string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeBroadcastRecipientCursor(token string) (database.BroadcastRecipientCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return database.BroadcastRecipientCursor{}, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return database.BroadcastRecipientCursor{}, errors.New("malformed cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	return database.BroadcastRecipientCursor{CreatedAt: t, ID: parts[1]}, err
}
