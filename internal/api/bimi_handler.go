package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Ferousco-dev/mailx/internal/bimi"
	"github.com/Ferousco-dev/mailx/internal/database"
)

// bimiHandler exposes sender-side BIMI readiness. It is derived from public
// DNS, MailX's own DMARC readiness, and (only when requested) a fetched
// logo/certificate, and stores nothing. It never grants ownership, changes
// DMARC/DKIM/SPF, blocks sending, or claims that any mailbox provider will
// actually display the logo — every response's disclaimer field says so
// explicitly.
type bimiHandler struct{ service *bimi.Service }

type bimiDNSResource struct {
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	Source     string `json:"source,omitempty"`
	RecordName string `json:"record_name,omitempty"`
	Published  string `json:"published_record,omitempty"`
	Location   string `json:"logo_location,omitempty"`
	Authority  string `json:"authority_location,omitempty"`
	Declined   bool   `json:"declined"`
}

type bimiDMARCResource struct {
	Checked              bool   `json:"checked"`
	EffectivePolicy      string `json:"effective_policy,omitempty"`
	OrganizationalDomain string `json:"organizational_domain,omitempty"`
}

type bimiLogoResource struct {
	Checked    bool     `json:"checked"`
	Valid      bool     `json:"valid,omitempty"`
	FetchError string   `json:"fetch_error,omitempty"`
	Reasons    []string `json:"reasons,omitempty"`
}

type bimiCertResource struct {
	Checked        bool   `json:"checked"`
	Parseable      bool   `json:"parseable,omitempty"`
	FetchError     string `json:"fetch_error,omitempty"`
	ParseError     string `json:"parse_error,omitempty"`
	Subject        string `json:"subject,omitempty"`
	Issuer         string `json:"issuer,omitempty"`
	CurrentlyValid bool   `json:"currently_valid,omitempty"`
}

type bimiResource struct {
	DomainID    string            `json:"domain_id"`
	Domain      string            `json:"domain"`
	Selector    string            `json:"selector"`
	Readiness   string            `json:"readiness"`
	DNS         bimiDNSResource   `json:"dns"`
	DMARC       bimiDMARCResource `json:"dmarc"`
	Logo        bimiLogoResource  `json:"logo"`
	Certificate bimiCertResource  `json:"certificate"`
	Disclaimer  string            `json:"disclaimer"`
}

func bimiResourceFrom(id string, r bimi.Result) bimiResource {
	return bimiResource{
		DomainID: id, Domain: r.Domain, Selector: r.Selector, Readiness: r.Readiness,
		DNS: bimiDNSResource{
			Status: string(r.DNS.Status), Reason: r.DNS.Reason, Source: r.DNS.Source, RecordName: r.DNS.RecordName,
			Published: r.DNS.Published, Location: r.DNS.Record.Location, Authority: r.DNS.Record.Authority, Declined: r.DNS.Record.Declined,
		},
		DMARC: bimiDMARCResource{Checked: r.DMARC.Checked, EffectivePolicy: r.DMARC.EffectivePolicy, OrganizationalDomain: r.DMARC.OrgDomain},
		Logo:  bimiLogoResource{Checked: r.Logo.Checked, Valid: r.Logo.SVG.Valid, FetchError: r.Logo.FetchErr, Reasons: r.Logo.SVG.Reasons},
		Certificate: bimiCertResource{
			Checked: r.Cert.Checked, Parseable: r.Cert.Cert.Parseable, FetchError: r.Cert.FetchErr, ParseError: r.Cert.Cert.ParseError,
			Subject: r.Cert.Cert.Subject, Issuer: r.Cert.Cert.Issuer, CurrentlyValid: r.Cert.Cert.CurrentlyValid,
		},
		Disclaimer: r.Disclaimer,
	}
}

func (h *bimiHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, database.ErrNotFound):
		writeError(w, r, newError(ErrNotFoundType, "domain_not_found", "no domain found with that id"))
	case errors.Is(err, bimi.ErrDomainNotVerified):
		writeError(w, r, newError(ErrConflictType, "domain_not_verified", "the domain must be verified before its BIMI readiness can be checked"))
	case errors.Is(err, bimi.ErrBusy), errors.Is(err, context.DeadlineExceeded):
		writeError(w, r, newError(ErrTemporarilyUnavailable, "bimi_verification_busy", "BIMI verification is busy; retry shortly"))
	default:
		writeError(w, r, newError(ErrInternal, "internal_error", "BIMI operation failed"))
	}
}

func (h *bimiHandler) available(w http.ResponseWriter, r *http.Request) bool {
	if h.service == nil {
		writeError(w, r, newError(ErrTemporarilyUnavailable, "bimi_not_configured", "BIMI guidance is not configured on this server"))
		return false
	}
	return true
}

// handleGet returns the identity model with NO DNS query.
func (h *bimiHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	if !h.available(w, r) {
		return
	}
	id := r.PathValue("id")
	res, err := h.service.Describe(r.Context(), tenantFromContext(r.Context()), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, bimiResourceFrom(id, res))
}

// handleVerify inspects public DNS (bounded) and reports readiness. Pass
// ?validate_assets=true to also fetch and structurally validate the
// referenced logo (and certificate, if published) — this adds up to two
// bounded outbound HTTPS fetches, so it is opt-in rather than the default.
// It changes no state.
func (h *bimiHandler) handleVerify(w http.ResponseWriter, r *http.Request) {
	if !h.available(w, r) {
		return
	}
	id := r.PathValue("id")
	fetchAssets := r.URL.Query().Get("validate_assets") == "true"
	res, err := h.service.Verify(r.Context(), tenantFromContext(r.Context()), id, fetchAssets)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, bimiResourceFrom(id, res))
}
