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
	"github.com/Ferousco-dev/mailx/internal/emailtemplate"
)

// templateHandler manages a tenant's reusable email templates. Every query is
// tenant-scoped, so another tenant's template is indistinguishable from a
// missing one (404) — never leaked via a different status code or message.
type templateHandler struct{ db *database.DB }

type createTemplateRequest struct {
	Name    string `json:"name"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
	HTML    string `json:"html"`
}

type updateTemplateRequest struct {
	Name    *string `json:"name"`
	Subject *string `json:"subject"`
	Text    *string `json:"text"`
	HTML    *string `json:"html"`
}

type templateResource struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Subject   string    `json:"subject"`
	Text      string    `json:"text"`
	HTML      string    `json:"html"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type templateList struct {
	Data       []templateResource `json:"data"`
	NextCursor *string            `json:"next_cursor"`
}

func templateFromRow(t database.Template) templateResource {
	return templateResource{ID: t.ID, Name: t.Name, Subject: t.Subject, Text: t.Text, HTML: t.HTML, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}

func templateValidationError(err error) *apiError {
	switch {
	case errors.Is(err, emailtemplate.ErrEmptyName):
		return newError(ErrValidation, "invalid_name", "name is required")
	case errors.Is(err, emailtemplate.ErrNameTooLong):
		return newError(ErrValidation, "invalid_name", fmt.Sprintf("name must be at most %d characters", emailtemplate.MaxNameLen))
	case errors.Is(err, emailtemplate.ErrEmptyBody):
		return newError(ErrValidation, "missing_body", "at least one of text or html is required")
	case errors.Is(err, emailtemplate.ErrTooLarge):
		return newError(ErrValidation, "body_too_large", "subject/text/html exceed the maximum size")
	default:
		return newError(ErrValidation, "invalid_template", err.Error())
	}
}

func (h *templateHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req createTemplateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_json", "request body is not valid JSON"))
		return
	}
	if err := emailtemplate.Validate(req.Name, req.Subject, req.Text, req.HTML); err != nil {
		writeError(w, r, templateValidationError(err))
		return
	}
	t, err := h.db.CreateTemplate(r.Context(), database.NewTemplate{
		TenantID: tenantFromContext(r.Context()), Name: req.Name, Subject: req.Subject, Text: req.Text, HTML: req.HTML,
	})
	switch {
	case errors.Is(err, database.ErrConflict):
		writeError(w, r, newError(ErrConflictType, "template_name_taken", "a template with this name already exists"))
	case err != nil:
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to create template"))
	default:
		writeJSON(w, http.StatusCreated, templateFromRow(t))
	}
}

func (h *templateHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	t, err := h.db.GetTemplate(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"))
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "template_not_found", "no template found with that id"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load template"))
		return
	}
	writeJSON(w, http.StatusOK, templateFromRow(t))
}

func (h *templateHandler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req updateTemplateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_json", "request body is not valid JSON"))
		return
	}
	tenantID := tenantFromContext(r.Context())
	current, err := h.db.GetTemplate(r.Context(), tenantID, r.PathValue("id"))
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "template_not_found", "no template found with that id"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load template"))
		return
	}
	name, subject, text, html := current.Name, current.Subject, current.Text, current.HTML
	if req.Name != nil {
		name = *req.Name
	}
	if req.Subject != nil {
		subject = *req.Subject
	}
	if req.Text != nil {
		text = *req.Text
	}
	if req.HTML != nil {
		html = *req.HTML
	}
	if err := emailtemplate.Validate(name, subject, text, html); err != nil {
		writeError(w, r, templateValidationError(err))
		return
	}
	updated, err := h.db.UpdateTemplate(r.Context(), tenantID, current.ID, database.TemplateUpdate{
		Name: &name, Subject: &subject, Text: &text, HTML: &html,
	})
	switch {
	case errors.Is(err, database.ErrNotFound): // deleted concurrently between the load and the update
		writeError(w, r, newError(ErrNotFoundType, "template_not_found", "no template found with that id"))
	case errors.Is(err, database.ErrConflict):
		writeError(w, r, newError(ErrConflictType, "template_name_taken", "a template with this name already exists"))
	case err != nil:
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to update template"))
	default:
		writeJSON(w, http.StatusOK, templateFromRow(updated))
	}
}

// handleDelete is a hard delete (v0.33: no versioning/soft-delete). Messages
// already accepted from this template are unaffected — see database.DeleteTemplate.
func (h *templateHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	err := h.db.DeleteTemplate(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"))
	switch {
	case errors.Is(err, database.ErrNotFound):
		writeError(w, r, newError(ErrNotFoundType, "template_not_found", "no template found with that id"))
	case err != nil:
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to delete template"))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *templateHandler) handleList(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, r, newError(ErrValidation, "invalid_limit", fmt.Sprintf("limit must be an integer between 1 and %d", maxLimit)))
			return
		}
		limit = n
	}
	var after *database.TemplateCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeTemplateCursor(raw)
		if err != nil {
			writeError(w, r, newError(ErrInvalidRequest, "invalid_cursor", "cursor is not valid"))
			return
		}
		after = &c
	}
	rows, err := h.db.ListTemplates(r.Context(), tenantFromContext(r.Context()), limit+1, after)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to list templates"))
		return
	}
	resp := templateList{Data: make([]templateResource, 0, min(len(rows), limit))}
	for _, row := range rows[:min(len(rows), limit)] {
		resp.Data = append(resp.Data, templateFromRow(row))
	}
	if len(rows) > limit {
		last := rows[limit-1]
		token := encodeTemplateCursor(database.TemplateCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		resp.NextCursor = &token
	}
	writeJSON(w, http.StatusOK, resp)
}

func encodeTemplateCursor(c database.TemplateCursor) string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeTemplateCursor(token string) (database.TemplateCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return database.TemplateCursor{}, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return database.TemplateCursor{}, errors.New("malformed cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	return database.TemplateCursor{CreatedAt: t, ID: parts[1]}, err
}
