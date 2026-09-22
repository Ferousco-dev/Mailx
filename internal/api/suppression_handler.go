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
	"github.com/Ferousco-dev/mailx/internal/observability"
	"github.com/Ferousco-dev/mailx/internal/suppression"
)

// suppressionHandler manages a tenant's recipient suppression list. Everything is
// tenant-scoped in the query itself, so another tenant's entries are
// indistinguishable from missing ones (404), and no response carries tenant ids,
// SQL text or other internal state.
type suppressionHandler struct {
	db      *database.DB
	metrics *observability.Metrics
}

type createSuppressionRequest struct {
	Email  string `json:"email"`
	Reason string `json:"reason"`
}

type suppressionResource struct {
	ID             string    `json:"id"`
	Email          string    `json:"email"`
	Reason         string    `json:"reason"`
	Source         string    `json:"source"`
	MessageID      *string   `json:"message_id,omitempty"`
	SMTPCode       *int      `json:"smtp_code,omitempty"`
	EnhancedStatus *string   `json:"enhanced_status,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type suppressionList struct {
	Data       []suppressionResource `json:"data"`
	NextCursor *string               `json:"next_cursor"`
}

func suppressionFromRow(s database.Suppression) suppressionResource {
	return suppressionResource{ID: s.ID, Email: s.Email, Reason: string(s.Reason), Source: string(s.Source),
		MessageID: s.MessageID, SMTPCode: s.SMTPCode, EnhancedStatus: s.EnhancedStatus, CreatedAt: s.CreatedAt}
}

// handleCreate is idempotent by construction: suppressing an address that is
// already suppressed returns the EXISTING entry unchanged (200), never an error and
// never a second row, so client retries and concurrent requests are safe. A new
// entry is 201.
func (h *suppressionHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req createSuppressionRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_json", "request body is not valid JSON"))
		return
	}
	reason, err := suppression.ParseAPIReason(req.Reason)
	if err != nil {
		writeError(w, r, newError(ErrValidation, "invalid_reason", "reason must be \"manual\""))
		return
	}
	created, wasCreated, err := h.db.CreateSuppression(r.Context(), database.NewSuppression{
		TenantID: tenantFromContext(r.Context()), Email: req.Email, Reason: reason, Source: suppression.SourceAPI,
	})
	switch {
	case errors.Is(err, database.ErrInvalidSuppression):
		writeError(w, r, newError(ErrValidation, "invalid_email", "email must be a plain ASCII address such as person@example.com"))
	case err != nil:
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to create suppression"))
	case wasCreated:
		h.metrics.SuppressionWrite(string(reason), string(suppression.SourceAPI))
		writeJSON(w, http.StatusCreated, suppressionFromRow(created))
	default:
		writeJSON(w, http.StatusOK, suppressionFromRow(created))
	}
}

func (h *suppressionHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	s, err := h.db.GetSuppression(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"))
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "suppression_not_found", "no suppression found with that id"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load suppression"))
		return
	}
	writeJSON(w, http.StatusOK, suppressionFromRow(s))
}

// handleDelete removes the entry (hard delete) so future sends may be attempted.
// It changes no history: past messages, attempts and events are untouched and
// nothing is re-queued.
func (h *suppressionHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	err := h.db.DeleteSuppression(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"))
	switch {
	case errors.Is(err, database.ErrNotFound):
		writeError(w, r, newError(ErrNotFoundType, "suppression_not_found", "no suppression found with that id"))
	case err != nil:
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to delete suppression"))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *suppressionHandler) handleList(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, r, newError(ErrValidation, "invalid_limit", fmt.Sprintf("limit must be an integer between 1 and %d", maxLimit)))
			return
		}
		limit = n
	}
	var after *database.SuppressionCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeSuppressionCursor(raw)
		if err != nil {
			writeError(w, r, newError(ErrInvalidRequest, "invalid_cursor", "cursor is not valid"))
			return
		}
		after = &c
	}
	rows, err := h.db.ListSuppressions(r.Context(), tenantFromContext(r.Context()), limit+1, after, r.URL.Query().Get("email"))
	if errors.Is(err, database.ErrInvalidSuppression) {
		writeError(w, r, newError(ErrValidation, "invalid_email", "email must be a plain ASCII address such as person@example.com"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to list suppressions"))
		return
	}
	resp := suppressionList{Data: make([]suppressionResource, 0, min(len(rows), limit))}
	for _, row := range rows[:min(len(rows), limit)] {
		resp.Data = append(resp.Data, suppressionFromRow(row))
	}
	if len(rows) > limit {
		last := rows[limit-1]
		token := encodeSuppressionCursor(database.SuppressionCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		resp.NextCursor = &token
	}
	writeJSON(w, http.StatusOK, resp)
}

func encodeSuppressionCursor(c database.SuppressionCursor) string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeSuppressionCursor(token string) (database.SuppressionCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return database.SuppressionCursor{}, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return database.SuppressionCursor{}, errors.New("malformed cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	return database.SuppressionCursor{CreatedAt: t, ID: parts[1]}, err
}

// checkRecipientsForAcceptance runs at POST /v1/emails, before signing and before
// anything is stored. It gives fast feedback and guarantees every accepted
// recipient has a suppression key: an address MailX cannot key could never be
// suppressed, so it is refused (422 invalid_recipient). If every recipient is
// suppressed nothing is stored (422 all_recipients_suppressed). Partially
// suppressed requests ARE accepted: the worker skips the suppressed recipients at
// delivery time and records that truthfully, and it re-checks anyway because a
// suppression can appear after acceptance. A failing lookup refuses the request
// (never "assume not suppressed").
//
// It returns the number of recipients that will actually be delivered (not
// suppressed): the recipient rate limit charges exactly that number, so a
// suppressed address costs the sender nothing.
func (h *emailHandler) checkRecipientsForAcceptance(ctx context.Context, tenantID string, envelope []string) (deliverable int, apiErr *apiError) {
	keys := make([]string, 0, len(envelope))
	for _, rcpt := range envelope {
		k, err := suppression.Normalize(rcpt)
		if err != nil {
			return 0, newError(ErrValidation, "invalid_recipient", "every recipient must be a plain ASCII address such as person@example.com")
		}
		keys = append(keys, k)
	}
	suppressed, err := h.db.SuppressedForTenant(ctx, tenantID, keys)
	if err != nil {
		return 0, newError(ErrInternal, "internal_error", "failed to check recipient suppression")
	}
	for _, k := range keys {
		if !suppressed[k] {
			deliverable++
		}
	}
	if deliverable > 0 {
		return deliverable, nil
	}
	return 0, newError(ErrValidation, "all_recipients_suppressed", "every recipient is on this account's suppression list; nothing was sent")
}
