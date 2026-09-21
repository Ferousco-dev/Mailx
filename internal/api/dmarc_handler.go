package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/dmarc"
)

// dmarcHandler exposes sender-side DMARC readiness. It is derived from public
// DNS and MailX's own DKIM/SPF facts on demand and stores nothing. It never
// grants ownership, changes DKIM or SPF, blocks sending, or reports a receiver's
// DMARC result (receiver_result is always "not_observed").
type dmarcHandler struct{ service *dmarc.Service }

type dmarcPathResource struct {
	Identity string `json:"identity"`
	Aligned  bool   `json:"aligned"`
	Mode     string `json:"mode"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
}

type dmarcDNSResource struct {
	Status          string `json:"status"`
	Reason          string `json:"reason,omitempty"`
	Source          string `json:"source,omitempty"`
	RecordName      string `json:"record_name,omitempty"`
	Policy          string `json:"policy,omitempty"`
	SubdomainPolicy string `json:"subdomain_policy,omitempty"`
	EffectivePolicy string `json:"effective_policy,omitempty"`
	Testing         bool   `json:"testing"`
	ReportURICount  int    `json:"report_uri_count"`
	Published       string `json:"published_record,omitempty"`
}

type dmarcResource struct {
	DomainID       string            `json:"domain_id"`
	Domain         string            `json:"domain"`
	OrgDomain      string            `json:"organizational_domain,omitempty"`
	Mode           string            `json:"mode,omitempty"`
	Checked        bool              `json:"checked"`
	Readiness      string            `json:"readiness"`
	ReceiverResult string            `json:"receiver_result"`
	DNS            dmarcDNSResource  `json:"dns"`
	DKIM           dmarcPathResource `json:"dkim"`
	SPF            dmarcPathResource `json:"spf"`
	Expected       spfRecordResource `json:"expected"`
	Warnings       []string          `json:"warnings"`
}

func dmarcPath(p dmarc.Path) dmarcPathResource {
	return dmarcPathResource{Identity: p.Identity, Aligned: p.Aligned, Mode: mode(p.Mode), Status: p.Status, Reason: p.Reason}
}

func mode(m dmarc.Mode) string {
	if m == dmarc.Strict {
		return "strict"
	}
	return "relaxed"
}

func dmarcResourceFrom(id string, r dmarc.Result) dmarcResource {
	out := dmarcResource{
		DomainID: id, Domain: r.Domain, OrgDomain: r.OrgDomain, Mode: r.Mode, Checked: r.Checked, Readiness: r.Readiness,
		ReceiverResult: "not_observed",
		DNS: dmarcDNSResource{Status: string(r.DNS.Status), Reason: r.DNS.Reason, Source: r.DNS.Source, RecordName: r.DNS.RecordName,
			Policy: string(r.DNS.Policy), SubdomainPolicy: string(r.DNS.SubdomainPolicy), EffectivePolicy: string(r.DNS.Effective),
			Testing: r.DNS.Testing, ReportURICount: r.DNS.ReportURIs, Published: r.DNS.Published},
		DKIM: dmarcPath(r.DKIM), SPF: dmarcPath(r.SPF), Warnings: r.Warnings,
		Expected: spfRecordResource{Action: r.Expected.Action, Type: r.Expected.Type, Name: r.Expected.Name, Value: r.Expected.Value},
	}
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	return out
}

func (h *dmarcHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, database.ErrNotFound):
		writeError(w, r, newError(ErrNotFoundType, "domain_not_found", "no domain found with that id"))
	case errors.Is(err, dmarc.ErrDomainNotVerified):
		writeError(w, r, newError(ErrConflictType, "domain_not_verified", "the domain must be verified before its DMARC can be checked"))
	case errors.Is(err, dmarc.ErrBusy), errors.Is(err, context.DeadlineExceeded):
		writeError(w, r, newError(ErrTemporarilyUnavailable, "dmarc_verification_busy", "DMARC verification is busy; retry shortly"))
	default:
		writeError(w, r, newError(ErrInternal, "internal_error", "DMARC operation failed"))
	}
}

func (h *dmarcHandler) available(w http.ResponseWriter, r *http.Request) bool {
	if h.service == nil {
		writeError(w, r, newError(ErrTemporarilyUnavailable, "dmarc_not_configured", "DMARC guidance is not configured on this server"))
		return false
	}
	return true
}

// handleGet returns the recommendation and identity model with NO DNS query.
func (h *dmarcHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	if !h.available(w, r) {
		return
	}
	id := r.PathValue("id")
	res, err := h.service.Describe(r.Context(), tenantFromContext(r.Context()), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, dmarcResourceFrom(id, res))
}

// handleVerify inspects public DNS (bounded) and reports readiness. It changes no state.
func (h *dmarcHandler) handleVerify(w http.ResponseWriter, r *http.Request) {
	if !h.available(w, r) {
		return
	}
	id := r.PathValue("id")
	res, err := h.service.Verify(r.Context(), tenantFromContext(r.Context()), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, dmarcResourceFrom(id, res))
}
