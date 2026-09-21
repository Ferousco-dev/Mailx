package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/spf"
)

// spfHandler exposes SPF guidance and verification. SPF state is derived from
// public DNS on demand and is never stored, so there is nothing to go stale.
// Nothing here grants, changes or reads From-domain authorization or DKIM state.
type spfHandler struct{ service *spf.Service }

type spfRecordResource struct {
	Action string `json:"action"`
	Type   string `json:"type,omitempty"`
	Name   string `json:"name,omitempty"`
	Value  string `json:"value,omitempty"`
}

type spfSendingResource struct {
	IPs          []string `json:"ips,omitempty"`
	RelayInclude string   `json:"relay_include,omitempty"`
}

type spfResource struct {
	DomainID    string             `json:"domain_id"`
	Domain      string             `json:"domain"`
	Mode        string             `json:"mode"`
	Checked     bool               `json:"checked"`
	Status      string             `json:"status"`
	Reason      string             `json:"reason,omitempty"`
	Sending     spfSendingResource `json:"sending"`
	Published   string             `json:"published_record,omitempty"`
	Expected    spfRecordResource  `json:"expected"`
	Warnings    []string           `json:"warnings"`
	Unevaluated bool               `json:"unevaluated_mechanisms"`
}

func spfResourceFrom(id string, cfg spf.Config, res spf.Result) spfResource {
	out := spfResource{
		DomainID: id, Domain: res.Domain, Mode: string(res.Mode), Checked: res.Status != spf.StatusUnchecked &&
			res.Status != spf.StatusSendingUnknown,
		Status: string(res.Status), Reason: res.Reason, Published: res.Published, Warnings: res.Warnings,
		Unevaluated: res.Unevaluated,
		Expected:    spfRecordResource{Action: res.Expected.Action, Type: res.Expected.Type, Name: res.Expected.Name, Value: res.Expected.Value},
		Sending:     spfSendingResource{RelayInclude: cfg.RelayInclude},
	}
	for _, ip := range cfg.IPs {
		out.Sending.IPs = append(out.Sending.IPs, ip.String())
	}
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	return out
}

func (h *spfHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, database.ErrNotFound):
		// Another tenant's domain and a missing one are indistinguishable.
		writeError(w, r, newError(ErrNotFoundType, "domain_not_found", "no domain found with that id"))
	case errors.Is(err, spf.ErrDomainNotVerified):
		writeError(w, r, newError(ErrConflictType, "domain_not_verified", "the domain must be verified before its SPF can be checked"))
	case errors.Is(err, spf.ErrBusy), errors.Is(err, context.DeadlineExceeded):
		writeError(w, r, newError(ErrTemporarilyUnavailable, "spf_verification_busy", "SPF verification is busy; retry shortly"))
	default:
		writeError(w, r, newError(ErrInternal, "internal_error", "SPF operation failed"))
	}
}

func (h *spfHandler) available(w http.ResponseWriter, r *http.Request) bool {
	if h.service == nil {
		writeError(w, r, newError(ErrTemporarilyUnavailable, "spf_not_configured", "SPF guidance is not configured on this server"))
		return false
	}
	return true
}

// handleGet returns what to publish. It performs no DNS query.
func (h *spfHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	if !h.available(w, r) {
		return
	}
	id := r.PathValue("id")
	res, err := h.service.Describe(r.Context(), tenantFromContext(r.Context()), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, spfResourceFrom(id, h.service.Config(), res))
}

// handleVerify performs one bounded public TXT lookup and reports the result.
// It changes no state.
func (h *spfHandler) handleVerify(w http.ResponseWriter, r *http.Request) {
	if !h.available(w, r) {
		return
	}
	id := r.PathValue("id")
	res, err := h.service.Verify(r.Context(), tenantFromContext(r.Context()), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, spfResourceFrom(id, h.service.Config(), res))
}
