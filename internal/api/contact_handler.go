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

	"github.com/Ferousco-dev/mailx/internal/contact"
	"github.com/Ferousco-dev/mailx/internal/database"
)

// contactHandler manages a tenant's contacts: "this tenant knows this
// recipient", independent of suppressions and message recipients. Every
// query is tenant-scoped; another tenant's contact is indistinguishable
// from a missing one (404).
type contactHandler struct{ db *database.DB }

type createContactRequest struct {
	Email      string            `json:"email"`
	Name       string            `json:"name"`
	Attributes map[string]string `json:"attributes"`
}

type updateContactRequest struct {
	Email      *string           `json:"email"`
	Name       *string           `json:"name"`
	Attributes map[string]string `json:"attributes"`
}

type contactResource struct {
	ID         string            `json:"id"`
	Email      string            `json:"email"`
	Name       string            `json:"name"`
	Attributes map[string]string `json:"attributes"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

type contactList struct {
	Data       []contactResource `json:"data"`
	NextCursor *string           `json:"next_cursor"`
}

func contactFromRow(c database.Contact) contactResource {
	attrs := c.Attributes
	if attrs == nil {
		attrs = map[string]string{}
	}
	return contactResource{ID: c.ID, Email: c.Email, Name: c.Name, Attributes: attrs, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

func (h *contactHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req createContactRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_json", "request body is not valid JSON"))
		return
	}
	if aerr := validateContactInput(req.Name, req.Attributes); aerr != nil {
		writeError(w, r, aerr)
		return
	}
	c, err := h.db.CreateContact(r.Context(), database.NewContact{
		TenantID: tenantFromContext(r.Context()), Email: req.Email, Name: req.Name, Attributes: req.Attributes,
	})
	switch {
	case errors.Is(err, database.ErrInvalidContact):
		writeError(w, r, newError(ErrValidation, "invalid_email", "email must be a plain ASCII address such as person@example.com"))
	case errors.Is(err, database.ErrConflict):
		writeError(w, r, newError(ErrConflictType, "contact_exists", "a contact with this email already exists"))
	case err != nil:
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to create contact"))
	default:
		writeJSON(w, http.StatusCreated, contactFromRow(c))
	}
}

func (h *contactHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	c, err := h.db.GetContact(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"))
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "contact_not_found", "no contact found with that id"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load contact"))
		return
	}
	writeJSON(w, http.StatusOK, contactFromRow(c))
}

func (h *contactHandler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req updateContactRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_json", "request body is not valid JSON"))
		return
	}
	name := ""
	if req.Name != nil {
		name = *req.Name
	}
	if aerr := validateContactInput(name, req.Attributes); aerr != nil {
		writeError(w, r, aerr)
		return
	}
	tenantID := tenantFromContext(r.Context())
	c, err := h.db.UpdateContact(r.Context(), tenantID, r.PathValue("id"), database.ContactUpdate{
		Email: req.Email, Name: req.Name, Attributes: req.Attributes,
	})
	switch {
	case errors.Is(err, database.ErrInvalidContact):
		writeError(w, r, newError(ErrValidation, "invalid_email", "email must be a plain ASCII address such as person@example.com"))
	case errors.Is(err, database.ErrNotFound):
		writeError(w, r, newError(ErrNotFoundType, "contact_not_found", "no contact found with that id"))
	case errors.Is(err, database.ErrConflict):
		writeError(w, r, newError(ErrConflictType, "contact_exists", "a contact with this email already exists"))
	case err != nil:
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to update contact"))
	default:
		writeJSON(w, http.StatusOK, contactFromRow(c))
	}
}

// handleDelete is a hard delete. It touches only the contact row: no
// suppression, no message/delivery/feedback history is affected.
func (h *contactHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	err := h.db.DeleteContact(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"))
	switch {
	case errors.Is(err, database.ErrNotFound):
		writeError(w, r, newError(ErrNotFoundType, "contact_not_found", "no contact found with that id"))
	case err != nil:
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to delete contact"))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *contactHandler) handleList(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, r, newError(ErrValidation, "invalid_limit", fmt.Sprintf("limit must be an integer between 1 and %d", maxLimit)))
			return
		}
		limit = n
	}
	var after *database.ContactCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeContactCursor(raw)
		if err != nil {
			writeError(w, r, newError(ErrInvalidRequest, "invalid_cursor", "cursor is not valid"))
			return
		}
		after = &c
	}
	rows, err := h.db.ListContacts(r.Context(), tenantFromContext(r.Context()), limit+1, after)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to list contacts"))
		return
	}
	resp := contactList{Data: make([]contactResource, 0, min(len(rows), limit))}
	for _, row := range rows[:min(len(rows), limit)] {
		resp.Data = append(resp.Data, contactFromRow(row))
	}
	if len(rows) > limit {
		last := rows[limit-1]
		token := encodeContactCursor(database.ContactCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		resp.NextCursor = &token
	}
	writeJSON(w, http.StatusOK, resp)
}

func validateContactInput(name string, attrs map[string]string) *apiError {
	if err := contact.ValidateName(name); err != nil {
		return newError(ErrValidation, "name_too_long", err.Error())
	}
	if err := contact.ValidateAttributes(attrs); err != nil {
		return newError(ErrValidation, "invalid_attributes", err.Error())
	}
	return nil
}

func encodeContactCursor(c database.ContactCursor) string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeContactCursor(token string) (database.ContactCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return database.ContactCursor{}, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return database.ContactCursor{}, errors.New("malformed cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	return database.ContactCursor{CreatedAt: t, ID: parts[1]}, err
}
