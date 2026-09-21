package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/webhook"
)

type webhookHandler struct {
	service *webhook.Service
	db      *database.DB
}

type createWebhookRequest struct {
	URL    string   `json:"url"`
	Events []string `json:"events"`
}

type webhookResource struct {
	ID            string    `json:"id"`
	URL           string    `json:"url"`
	Events        []string  `json:"events"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	SigningSecret string    `json:"signing_secret,omitempty"`
}

type webhookList struct {
	Data       []webhookResource `json:"data"`
	NextCursor *string           `json:"next_cursor"`
}

type webhookDeliveryResource struct {
	ID                string     `json:"id"`
	EventID           string     `json:"event_id"`
	Status            string     `json:"status"`
	AttemptCount      int        `json:"attempt_count"`
	NextAttemptAt     time.Time  `json:"next_attempt_at"`
	LastErrorCategory *string    `json:"last_error_category,omitempty"`
	LastResponseCode  *int       `json:"last_response_code,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	DeliveredAt       *time.Time `json:"delivered_at,omitempty"`
	FailedAt          *time.Time `json:"failed_at,omitempty"`
}

type webhookDeliveryList struct {
	Data       []webhookDeliveryResource `json:"data"`
	NextCursor *string                   `json:"next_cursor"`
}

func webhookFromRow(row database.WebhookSubscription) webhookResource {
	return webhookResource{ID: row.ID, URL: row.URL, Events: row.EventTypes, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func (h *webhookHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req createWebhookRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_json", "request body is not valid JSON"))
		return
	}
	created, err := h.service.Create(r.Context(), tenantFromContext(r.Context()), req.URL, req.Events)
	if err != nil {
		if errors.Is(err, webhook.ErrInvalidURL) || strings.Contains(err.Error(), "event type") {
			writeError(w, r, newError(ErrValidation, "invalid_webhook", err.Error()))
			return
		}
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to create webhook"))
		return
	}
	resource := webhookFromRow(created.Subscription)
	resource.SigningSecret = created.Secret
	writeJSON(w, http.StatusCreated, resource)
}

func (h *webhookHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	row, err := h.service.Get(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"))
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "webhook_not_found", "no webhook found with that id"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load webhook"))
		return
	}
	writeJSON(w, http.StatusOK, webhookFromRow(row))
}

func (h *webhookHandler) handleList(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseListLimit(w, r)
	if !ok {
		return
	}
	var after *database.WebhookCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		createdAt, id, err := decodeTimeCursor(raw)
		if err != nil {
			writeError(w, r, newError(ErrInvalidRequest, "invalid_cursor", "cursor is not valid"))
			return
		}
		after = &database.WebhookCursor{CreatedAt: createdAt, ID: id}
	}
	rows, err := h.service.List(r.Context(), tenantFromContext(r.Context()), limit+1, after)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to list webhooks"))
		return
	}
	resp := webhookList{Data: make([]webhookResource, 0, min(len(rows), limit))}
	for _, row := range rows[:min(len(rows), limit)] {
		resp.Data = append(resp.Data, webhookFromRow(row))
	}
	if len(rows) > limit {
		last := rows[limit-1]
		cursor := encodeTimeCursor(last.CreatedAt, last.ID)
		resp.NextCursor = &cursor
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *webhookHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	err := h.service.Delete(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"))
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "webhook_not_found", "no webhook found with that id"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to delete webhook"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *webhookHandler) handleRotate(w http.ResponseWriter, r *http.Request) {
	created, err := h.service.RotateSecret(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"))
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "webhook_not_found", "no webhook found with that id"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to rotate webhook secret"))
		return
	}
	resource := webhookFromRow(created.Subscription)
	resource.SigningSecret = created.Secret
	writeJSON(w, http.StatusOK, resource)
}

func (h *webhookHandler) handleDeliveries(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseListLimit(w, r)
	if !ok {
		return
	}
	var after *database.WebhookDeliveryCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		createdAt, id, err := decodeTimeCursor(raw)
		if err != nil {
			writeError(w, r, newError(ErrInvalidRequest, "invalid_cursor", "cursor is not valid"))
			return
		}
		after = &database.WebhookDeliveryCursor{CreatedAt: createdAt, ID: id}
	}
	rows, err := h.service.ListDeliveries(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"), limit+1, after)
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "webhook_not_found", "no webhook found with that id"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to list webhook deliveries"))
		return
	}
	resp := webhookDeliveryList{Data: make([]webhookDeliveryResource, 0, min(len(rows), limit))}
	for _, row := range rows[:min(len(rows), limit)] {
		resp.Data = append(resp.Data, webhookDeliveryResource{
			ID: row.ID, EventID: row.EventID, Status: row.Status, AttemptCount: row.AttemptCount,
			NextAttemptAt: row.NextAttemptAt, LastErrorCategory: row.LastErrorCategory,
			LastResponseCode: row.LastResponseCode, CreatedAt: row.CreatedAt,
			DeliveredAt: row.DeliveredAt, FailedAt: row.FailedAt,
		})
	}
	if len(rows) > limit {
		last := rows[limit-1]
		cursor := encodeTimeCursor(last.CreatedAt, last.ID)
		resp.NextCursor = &cursor
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *webhookHandler) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseListLimit(w, r)
	if !ok {
		return
	}
	var after *database.EventCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		occurredAt, id, err := decodeTimeCursor(raw)
		if err != nil {
			writeError(w, r, newError(ErrInvalidRequest, "invalid_cursor", "cursor is not valid"))
			return
		}
		after = &database.EventCursor{OccurredAt: occurredAt, ID: id}
	}
	rows, err := h.db.ListTenantPublicEvents(r.Context(), tenantFromContext(r.Context()), limit+1, after)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to list events"))
		return
	}
	data := make([]map[string]any, 0, limit)
	for _, event := range rows[:min(len(rows), limit)] {
		publicType, visible := database.PublicEventType(event.Type)
		if !visible {
			continue
		}
		data = append(data, map[string]any{
			"id": event.ID, "type": publicType, "api_version": webhook.APIVersion,
			"created_at": event.OccurredAt,
			"data":       map[string]any{"email_id": event.MessageID},
		})
	}
	resp := map[string]any{"data": data, "next_cursor": nil}
	if len(rows) > limit {
		last := rows[limit-1]
		resp["next_cursor"] = encodeTimeCursor(last.OccurredAt, last.ID)
	}
	writeJSON(w, http.StatusOK, resp)
}

func parseListLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	limit := defaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, r, newError(ErrValidation, "invalid_limit", fmt.Sprintf("limit must be an integer between 1 and %d", maxLimit)))
			return 0, false
		}
		limit = n
	}
	return limit, true
}

func encodeTimeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeTimeCursor(token string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, "", err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return time.Time{}, "", errors.New("malformed cursor")
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	return at, parts[1], err
}
