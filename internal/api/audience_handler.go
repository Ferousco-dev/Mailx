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
)

const maxAudienceNameLen = 200

// audienceHandler manages audiences (named groups of existing Contacts) and
// their membership. v0.35 is membership only: no sending, no queue jobs, no
// template/MIME/DKIM/SMTP involvement anywhere in this file.
type audienceHandler struct{ db *database.DB }

type createAudienceRequest struct {
	Name string `json:"name"`
}

type updateAudienceRequest struct {
	Name string `json:"name"`
}

type addMemberRequest struct {
	ContactID string `json:"contact_id"`
}

type audienceResource struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type audienceList struct {
	Data       []audienceResource `json:"data"`
	NextCursor *string            `json:"next_cursor"`
}

func audienceFromRow(a database.Audience) audienceResource {
	return audienceResource{ID: a.ID, Name: a.Name, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt}
}

func validateAudienceName(name string) *apiError {
	if strings.TrimSpace(name) == "" {
		return newError(ErrValidation, "invalid_name", "name is required")
	}
	if len(name) > maxAudienceNameLen {
		return newError(ErrValidation, "invalid_name", fmt.Sprintf("name must be at most %d characters", maxAudienceNameLen))
	}
	return nil
}

func (h *audienceHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req createAudienceRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_json", "request body is not valid JSON"))
		return
	}
	if aerr := validateAudienceName(req.Name); aerr != nil {
		writeError(w, r, aerr)
		return
	}
	a, err := h.db.CreateAudience(r.Context(), database.NewAudience{TenantID: tenantFromContext(r.Context()), Name: req.Name})
	switch {
	case errors.Is(err, database.ErrConflict):
		writeError(w, r, newError(ErrConflictType, "audience_name_taken", "an audience with this name already exists"))
	case err != nil:
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to create audience"))
	default:
		writeJSON(w, http.StatusCreated, audienceFromRow(a))
	}
}

func (h *audienceHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	a, err := h.db.GetAudience(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"))
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "audience_not_found", "no audience found with that id"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load audience"))
		return
	}
	writeJSON(w, http.StatusOK, audienceFromRow(a))
}

func (h *audienceHandler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req updateAudienceRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_json", "request body is not valid JSON"))
		return
	}
	if aerr := validateAudienceName(req.Name); aerr != nil {
		writeError(w, r, aerr)
		return
	}
	a, err := h.db.UpdateAudience(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"), req.Name)
	switch {
	case errors.Is(err, database.ErrNotFound):
		writeError(w, r, newError(ErrNotFoundType, "audience_not_found", "no audience found with that id"))
	case errors.Is(err, database.ErrConflict):
		writeError(w, r, newError(ErrConflictType, "audience_name_taken", "an audience with this name already exists"))
	case err != nil:
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to update audience"))
	default:
		writeJSON(w, http.StatusOK, audienceFromRow(a))
	}
}

// handleDelete removes the audience and its membership rows (DB cascade) —
// never the underlying contacts, suppressions, or any delivery history.
func (h *audienceHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	err := h.db.DeleteAudience(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"))
	switch {
	case errors.Is(err, database.ErrNotFound):
		writeError(w, r, newError(ErrNotFoundType, "audience_not_found", "no audience found with that id"))
	case err != nil:
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to delete audience"))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *audienceHandler) handleList(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, r, newError(ErrValidation, "invalid_limit", fmt.Sprintf("limit must be an integer between 1 and %d", maxLimit)))
			return
		}
		limit = n
	}
	var after *database.AudienceCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeAudienceCursor(raw)
		if err != nil {
			writeError(w, r, newError(ErrInvalidRequest, "invalid_cursor", "cursor is not valid"))
			return
		}
		after = &c
	}
	rows, err := h.db.ListAudiences(r.Context(), tenantFromContext(r.Context()), limit+1, after)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to list audiences"))
		return
	}
	resp := audienceList{Data: make([]audienceResource, 0, min(len(rows), limit))}
	for _, row := range rows[:min(len(rows), limit)] {
		resp.Data = append(resp.Data, audienceFromRow(row))
	}
	if len(rows) > limit {
		last := rows[limit-1]
		token := encodeAudienceCursor(database.AudienceCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		resp.NextCursor = &token
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAddMember requires BOTH the audience and the contact to belong to the
// caller's tenant (enforced in database.AddAudienceMember); a foreign or
// unknown id is 404 either way, never disclosing which.
func (h *audienceHandler) handleAddMember(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req addMemberRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_json", "request body is not valid JSON"))
		return
	}
	if strings.TrimSpace(req.ContactID) == "" {
		writeError(w, r, newError(ErrValidation, "missing_contact_id", "contact_id is required"))
		return
	}
	tenantID := tenantFromContext(r.Context())
	err := h.db.AddAudienceMember(r.Context(), tenantID, r.PathValue("id"), req.ContactID)
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "not_found", "the audience or contact was not found"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to add member"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRemoveMember removes ONLY the membership row: the contact, its
// suppression state, and all historical data are untouched.
func (h *audienceHandler) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	err := h.db.RemoveAudienceMember(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"), r.PathValue("contact_id"))
	switch {
	case errors.Is(err, database.ErrNotFound):
		writeError(w, r, newError(ErrNotFoundType, "not_found", "the membership was not found"))
	case err != nil:
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to remove member"))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *audienceHandler) handleListMembers(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, r, newError(ErrValidation, "invalid_limit", fmt.Sprintf("limit must be an integer between 1 and %d", maxLimit)))
			return
		}
		limit = n
	}
	var after *database.MembershipCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeMembershipCursor(raw)
		if err != nil {
			writeError(w, r, newError(ErrInvalidRequest, "invalid_cursor", "cursor is not valid"))
			return
		}
		after = &c
	}
	rows, err := h.db.ListAudienceMembers(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"), limit+1, after)
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "audience_not_found", "no audience found with that id"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to list audience members"))
		return
	}
	resp := contactList{Data: make([]contactResource, 0, min(len(rows), limit))}
	for _, row := range rows[:min(len(rows), limit)] {
		resp.Data = append(resp.Data, contactFromRow(row))
	}
	if len(rows) > limit {
		last := rows[limit-1]
		token := encodeMembershipCursor(database.MembershipCursor{CreatedAt: last.CreatedAt, ContactID: last.ID})
		resp.NextCursor = &token
	}
	writeJSON(w, http.StatusOK, resp)
}

func encodeAudienceCursor(c database.AudienceCursor) string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeAudienceCursor(token string) (database.AudienceCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return database.AudienceCursor{}, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return database.AudienceCursor{}, errors.New("malformed cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	return database.AudienceCursor{CreatedAt: t, ID: parts[1]}, err
}

func encodeMembershipCursor(c database.MembershipCursor) string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ContactID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeMembershipCursor(token string) (database.MembershipCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return database.MembershipCursor{}, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return database.MembershipCursor{}, errors.New("malformed cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	return database.MembershipCursor{CreatedAt: t, ContactID: parts[1]}, err
}
