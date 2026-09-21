package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/dkim"
)

// dkimHandler exposes tenant-scoped DKIM key management. It never returns
// private key material, in any response, ever: the service does not hand it
// out, and the resource types below have no field that could carry it.
type dkimHandler struct{ service *dkim.Service }

type dkimDNS struct {
	Type        string   `json:"type"`
	Name        string   `json:"name"`
	Value       string   `json:"value"`
	ValueChunks []string `json:"value_chunks"`
}

type dkimKeyResource struct {
	Selector    string     `json:"selector"`
	Algorithm   string     `json:"algorithm"`
	KeyBits     int        `json:"key_bits"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	ActivatedAt *time.Time `json:"activated_at"`
	RetiredAt   *time.Time `json:"retired_at"`
	DNS         dkimDNS    `json:"dns"`
}

type dkimStatusResource struct {
	DomainID string            `json:"domain_id"`
	Domain   string            `json:"domain"`
	Signing  bool              `json:"signing"`
	Active   *dkimKeyResource  `json:"active"`
	Pending  *dkimKeyResource  `json:"pending"`
	Retired  []dkimKeyResource `json:"retired"`
}

type dkimVerifyResource struct {
	Published bool               `json:"published"`
	Status    dkimStatusResource `json:"status"`
}

func keyResource(domain string, k database.DKIMKey) dkimKeyResource {
	value := dkim.DNSValue(k.PublicKey)
	return dkimKeyResource{
		Selector: k.Selector, Algorithm: k.Algorithm, KeyBits: k.KeyBits, Status: string(k.Status),
		CreatedAt: k.CreatedAt, ActivatedAt: k.ActivatedAt, RetiredAt: k.RetiredAt,
		DNS: dkimDNS{Type: "TXT", Name: dkim.DNSName(k.Selector, domain), Value: value, ValueChunks: dkim.DNSChunks(value)},
	}
}

func statusResource(dom database.Domain, keys []database.DKIMKey) dkimStatusResource {
	out := dkimStatusResource{DomainID: dom.ID, Domain: dom.Name, Retired: []dkimKeyResource{}}
	for _, k := range keys {
		res := keyResource(dom.Name, k)
		switch k.Status {
		case database.DKIMActive:
			out.Active, out.Signing = &res, true
		case database.DKIMPending:
			out.Pending = &res
		default:
			out.Retired = append(out.Retired, res)
		}
	}
	return out
}

// fail maps service errors to API errors. Another tenant's domain and a missing
// one are indistinguishable (404).
func (h *dkimHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, database.ErrNotFound):
		writeError(w, r, newError(ErrNotFoundType, "domain_not_found", "no domain found with that id"))
	case errors.Is(err, database.ErrDomainNotVerified):
		writeError(w, r, newError(ErrConflictType, "domain_not_verified", "the domain must be verified before DKIM can be set up"))
	case errors.Is(err, database.ErrDKIMKeyPending):
		writeError(w, r, newError(ErrConflictType, "dkim_key_pending", "a DKIM key is already awaiting publication; verify it first"))
	case errors.Is(err, dkim.ErrDNSUnavailable):
		writeError(w, r, newError(ErrTemporarilyUnavailable, "dns_lookup_failed", "DNS verification is temporarily unavailable; retry later"))
	default:
		writeError(w, r, newError(ErrInternal, "internal_error", "DKIM operation failed"))
	}
}

func (h *dkimHandler) available(w http.ResponseWriter, r *http.Request) bool {
	if h.service == nil {
		writeError(w, r, newError(ErrTemporarilyUnavailable, "dkim_not_configured", "DKIM is not configured on this server"))
		return false
	}
	return true
}

func (h *dkimHandler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !h.available(w, r) {
		return
	}
	dom, keys, err := h.service.Keys(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, statusResource(dom, keys))
}

func (h *dkimHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !h.available(w, r) {
		return
	}
	tenantID := tenantFromContext(r.Context())
	if _, _, err := h.service.CreateKey(r.Context(), tenantID, r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	dom, keys, err := h.service.Keys(r.Context(), tenantID, r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, statusResource(dom, keys))
}

func (h *dkimHandler) handleVerify(w http.ResponseWriter, r *http.Request) {
	if !h.available(w, r) {
		return
	}
	tenantID := tenantFromContext(r.Context())
	_, published, err := h.service.VerifyPending(r.Context(), tenantID, r.PathValue("id"))
	if errors.Is(err, database.ErrNotFound) {
		// Distinguish "no such domain" from "no pending key" without leaking
		// other tenants' domains: the domain lookup is tenant-scoped.
		_, _, kerr := h.service.Keys(r.Context(), tenantID, r.PathValue("id"))
		switch {
		case kerr == nil:
			writeError(w, r, newError(ErrConflictType, "dkim_no_pending_key", "there is no pending DKIM key to verify; create one first"))
			return
		case !errors.Is(kerr, database.ErrNotFound):
			// A failing lookup is an infrastructure error, not a missing domain.
			h.fail(w, r, kerr)
			return
		}
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	dom, keys, err := h.service.Keys(r.Context(), tenantID, r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, dkimVerifyResource{Published: published, Status: statusResource(dom, keys)})
}
