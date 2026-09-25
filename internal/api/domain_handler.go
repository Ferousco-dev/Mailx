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
	maildomain "github.com/Ferousco-dev/mailx/internal/domain"
)

type domainHandler struct {
	service *maildomain.Service
	db      *database.DB // for the plan domain cap; nil skips it
}

func newDomainHandler(service *maildomain.Service) *domainHandler {
	return &domainHandler{service: service}
}

type createDomainRequest struct {
	Name string `json:"name"`
}

type dnsRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

type domainResource struct {
	ID             string      `json:"id"`
	Name           string      `json:"name"`
	OwnershipState string      `json:"ownership_state"`
	Records        []dnsRecord `json:"records"`
	CreatedAt      time.Time   `json:"created_at"`
	VerifiedAt     *time.Time  `json:"verified_at,omitempty"`
	LastCheckedAt  *time.Time  `json:"last_checked_at,omitempty"`
}

type domainList struct {
	Data       []domainResource `json:"data"`
	NextCursor *string          `json:"next_cursor"`
}

func domainFromRow(d database.Domain) domainResource {
	return domainResource{
		ID: d.ID, Name: d.Name, OwnershipState: string(d.VerificationStatus),
		Records:   []dnsRecord{{Type: "TXT", Name: maildomain.RecordName(d.Name), Value: maildomain.RecordValue(d.VerificationToken)}},
		CreatedAt: d.CreatedAt, VerifiedAt: d.VerifiedAt, LastCheckedAt: d.LastCheckedAt,
	}
}

func (h *domainHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req createDomainRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_json", "request body is not valid JSON"))
		return
	}
	if h.db != nil {
		if err := h.db.CheckDomainLimit(r.Context(), tenantFromContext(r.Context())); err != nil {
			if aerr := planLimitAPIError(err, false); aerr != nil {
				writeError(w, r, aerr)
				return
			}
			writeError(w, r, newError(ErrInternal, "internal_error", "failed to check plan limits"))
			return
		}
	}
	created, err := h.service.Create(r.Context(), tenantFromContext(r.Context()), req.Name)
	switch {
	case errors.Is(err, maildomain.ErrInvalidName):
		writeError(w, r, newError(ErrValidation, "invalid_domain", "name must be a valid public ASCII domain name"))
	case errors.Is(err, maildomain.ErrAlreadyExists):
		writeError(w, r, newError(ErrConflictType, "domain_already_exists", "this tenant already has an active resource for that domain"))
	case err != nil:
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to create domain"))
	default:
		writeJSON(w, http.StatusCreated, domainFromRow(created))
	}
}

func (h *domainHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	d, err := h.service.Get(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"))
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "domain_not_found", "no domain found with that id"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load domain"))
		return
	}
	writeJSON(w, http.StatusOK, domainFromRow(d))
}

func (h *domainHandler) handleList(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, r, newError(ErrValidation, "invalid_limit", fmt.Sprintf("limit must be an integer between 1 and %d", maxLimit)))
			return
		}
		limit = n
	}
	var after *database.DomainCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		cursor, err := decodeDomainCursor(raw)
		if err != nil {
			writeError(w, r, newError(ErrInvalidRequest, "invalid_cursor", "cursor is not valid"))
			return
		}
		after = &cursor
	}
	rows, err := h.service.List(r.Context(), tenantFromContext(r.Context()), limit+1, after)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to list domains"))
		return
	}
	resp := domainList{Data: make([]domainResource, 0, min(len(rows), limit))}
	for _, row := range rows[:min(len(rows), limit)] {
		resp.Data = append(resp.Data, domainFromRow(row))
	}
	if len(rows) > limit {
		last := rows[limit-1]
		token := encodeDomainCursor(database.DomainCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		resp.NextCursor = &token
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *domainHandler) handleVerify(w http.ResponseWriter, r *http.Request) {
	d, err := h.service.Verify(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"))
	switch {
	case errors.Is(err, database.ErrNotFound):
		writeError(w, r, newError(ErrNotFoundType, "domain_not_found", "no domain found with that id"))
	case errors.Is(err, maildomain.ErrOwnershipConflict):
		writeError(w, r, newError(ErrConflictType, "domain_ownership_conflict", "this domain is already verified by another account"))
	case errors.Is(err, maildomain.ErrDNSUnavailable):
		writeError(w, r, newError(ErrTemporarilyUnavailable, "dns_lookup_failed", "DNS verification is temporarily unavailable; retry later"))
	case err != nil:
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to verify domain"))
	default:
		writeJSON(w, http.StatusOK, domainFromRow(d))
	}
}

func (h *domainHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	err := h.service.Delete(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"))
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "domain_not_found", "no domain found with that id"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to delete domain"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func encodeDomainCursor(c database.DomainCursor) string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeDomainCursor(token string) (database.DomainCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return database.DomainCursor{}, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return database.DomainCursor{}, errors.New("malformed cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	return database.DomainCursor{CreatedAt: t, ID: parts[1]}, err
}
