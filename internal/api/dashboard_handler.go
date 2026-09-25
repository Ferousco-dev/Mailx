package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
)

// dashboardHandler serves the human-JWT dashboard surface (v0.47 phase 3a):
// /v1/me and the per-org detail/member/invite/analytics routes. Every route
// sits behind humanAuthMiddleware. Authorization posture (DEC-228): a
// non-member always gets 404 organization_not_found (no enumeration); a
// member who is not an owner gets 403 not_org_owner on owner-only routes.
type dashboardHandler struct {
	db  *database.DB
	now func() time.Time
}

const (
	maxDisplayNameLen = 200
	maxURLLen         = 2048
)

// orgAccess resolves the caller and the {id} org, enforcing membership (and
// ownership when ownerOnly). It writes the error response and returns ok=false
// on any failure.
func (h *dashboardHandler) orgAccess(w http.ResponseWriter, r *http.Request, ownerOnly bool) (humanID, tenantID string, ok bool) {
	humanID, ok = humanIDFromContext(r.Context())
	if !ok {
		writeError(w, r, newError(ErrAuthentication, "invalid_access_token", "missing or invalid access token"))
		return "", "", false
	}
	tenantID = r.PathValue("id")
	member, err := h.db.IsTenantMember(r.Context(), tenantID, humanID)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to check organization membership"))
		return "", "", false
	}
	if !member {
		writeError(w, r, newError(ErrNotFoundType, "organization_not_found", "organization not found"))
		return "", "", false
	}
	if ownerOnly {
		owner, err := h.db.IsTenantOwner(r.Context(), tenantID, humanID)
		if err != nil {
			writeError(w, r, newError(ErrInternal, "internal_error", "failed to check organization ownership"))
			return "", "", false
		}
		if !owner {
			writeError(w, r, newError(ErrForbidden, "not_org_owner", "only an organization owner can do this"))
			return "", "", false
		}
	}
	return humanID, tenantID, true
}

// patchBody is the shared {name?, <url field>?} PATCH shape. A present
// empty URL clears it; an absent field is left unchanged.
type patchBody struct {
	Name      *string `json:"name"`
	AvatarURL *string `json:"avatar_url"`
	LogoURL   *string `json:"logo_url"`
}

func decodePatch(w http.ResponseWriter, r *http.Request) (patchBody, *apiError) {
	var p patchBody
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		return p, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&p); err != nil {
		return p, newError(ErrInvalidRequest, "invalid_json", "request body is not valid JSON")
	}
	if p.Name != nil {
		n := strings.TrimSpace(*p.Name)
		if n == "" || len(n) > maxDisplayNameLen {
			return p, newError(ErrValidation, "invalid_name", "name must be 1-200 characters")
		}
		p.Name = &n
	}
	for _, u := range []*string{p.AvatarURL, p.LogoURL} {
		if u != nil && *u != "" && !validHTTPURL(*u) {
			return p, newError(ErrValidation, "invalid_url", "URL must be an absolute http(s) URL of at most 2048 characters")
		}
	}
	return p, nil
}

func validHTTPURL(s string) bool {
	if len(s) > maxURLLen {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

func rfc3339Ptr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

type meResponse struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Email         string        `json:"email"`
	AvatarURL     *string       `json:"avatar_url"`
	LastLoginAt   *string       `json:"last_login_at"`
	CreatedAt     string        `json:"created_at"`
	Organizations []orgResource `json:"organizations"`
}

func (h *dashboardHandler) writeMe(w http.ResponseWriter, r *http.Request, humanID string) {
	hu, err := h.db.GetHuman(r.Context(), humanID)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			writeError(w, r, newError(ErrAuthentication, "invalid_access_token", "missing or invalid access token"))
			return
		}
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load profile"))
		return
	}
	tenants, err := h.db.ListOrganizationsForHuman(r.Context(), humanID)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to list organizations"))
		return
	}
	orgs := make([]orgResource, 0, len(tenants))
	for _, t := range tenants {
		orgs = append(orgs, orgResourceFrom(t))
	}
	writeJSON(w, http.StatusOK, meResponse{
		ID: hu.ID, Name: hu.Name, Email: hu.Email, AvatarURL: hu.AvatarURL,
		LastLoginAt: rfc3339Ptr(hu.LastLoginAt), CreatedAt: hu.CreatedAt.UTC().Format(time.RFC3339),
		Organizations: orgs,
	})
}

func (h *dashboardHandler) handleGetMe(w http.ResponseWriter, r *http.Request) {
	humanID, ok := humanIDFromContext(r.Context())
	if !ok {
		writeError(w, r, newError(ErrAuthentication, "invalid_access_token", "missing or invalid access token"))
		return
	}
	h.writeMe(w, r, humanID)
}

func (h *dashboardHandler) handlePatchMe(w http.ResponseWriter, r *http.Request) {
	humanID, ok := humanIDFromContext(r.Context())
	if !ok {
		writeError(w, r, newError(ErrAuthentication, "invalid_access_token", "missing or invalid access token"))
		return
	}
	p, aerr := decodePatch(w, r)
	if aerr != nil {
		writeError(w, r, aerr)
		return
	}
	if p.LogoURL != nil {
		writeError(w, r, newError(ErrValidation, "invalid_field", "only name and avatar_url can be updated"))
		return
	}
	if err := h.db.UpdateHumanProfile(r.Context(), humanID, p.Name, p.AvatarURL); err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to update profile"))
		return
	}
	h.writeMe(w, r, humanID)
}

type orgDetailResponse struct {
	ID                   string  `json:"id"`
	Name                 string  `json:"name"`
	LogoURL              *string `json:"logo_url"`
	Plan                 string  `json:"plan"`
	PlanStatus           string  `json:"plan_status"`
	PlanCurrentPeriodEnd *string `json:"plan_current_period_end"`
	CreatedAt            string  `json:"created_at"`
}

func (h *dashboardHandler) writeOrg(w http.ResponseWriter, r *http.Request, tenantID string) {
	t, err := h.db.GetTenant(r.Context(), tenantID)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load organization"))
		return
	}
	tp, err := h.db.GetTenantPlan(r.Context(), tenantID)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load plan"))
		return
	}
	writeJSON(w, http.StatusOK, orgDetailResponse{
		ID: t.ID, Name: t.Name, LogoURL: t.LogoURL, Plan: tp.Plan, PlanStatus: tp.Status,
		PlanCurrentPeriodEnd: rfc3339Ptr(tp.CurrentPeriodEnd), CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
	})
}

func (h *dashboardHandler) handleGetOrg(w http.ResponseWriter, r *http.Request) {
	if _, tenantID, ok := h.orgAccess(w, r, false); ok {
		h.writeOrg(w, r, tenantID)
	}
}

func (h *dashboardHandler) handlePatchOrg(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.orgAccess(w, r, true)
	if !ok {
		return
	}
	p, aerr := decodePatch(w, r)
	if aerr != nil {
		writeError(w, r, aerr)
		return
	}
	if p.AvatarURL != nil {
		writeError(w, r, newError(ErrValidation, "invalid_field", "only name and logo_url can be updated"))
		return
	}
	if err := h.db.UpdateOrganization(r.Context(), tenantID, p.Name, p.LogoURL); err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to update organization"))
		return
	}
	h.writeOrg(w, r, tenantID)
}

type memberResource struct {
	HumanID   string  `json:"human_id"`
	Name      string  `json:"name"`
	Email     string  `json:"email"`
	AvatarURL *string `json:"avatar_url"`
	Role      string  `json:"role"`
	JoinedAt  string  `json:"joined_at"`
}

func (h *dashboardHandler) handleListMembers(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.orgAccess(w, r, false)
	if !ok {
		return
	}
	ms, err := h.db.ListTenantMembers(r.Context(), tenantID)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to list members"))
		return
	}
	out := make([]memberResource, 0, len(ms))
	for _, m := range ms {
		out = append(out, memberResource{HumanID: m.HumanID, Name: m.Name, Email: m.Email, AvatarURL: m.AvatarURL, Role: m.Role, JoinedAt: m.JoinedAt.UTC().Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (h *dashboardHandler) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	humanID, tenantID, ok := h.orgAccess(w, r, true)
	if !ok {
		return
	}
	err := h.db.RemoveTenantMember(r.Context(), tenantID, humanID, r.PathValue("humanId"))
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, database.ErrCannotRemoveSelf):
		writeError(w, r, newError(ErrConflictType, "cannot_remove_self", "owners cannot remove themselves; leaving or transferring ownership is not supported yet"))
	case errors.Is(err, database.ErrLastOwner):
		writeError(w, r, newError(ErrConflictType, "last_owner", "an organization must keep at least one owner"))
	case errors.Is(err, database.ErrNotTenantOwner):
		// Lost ownership between the pre-check and the locked re-check.
		writeError(w, r, newError(ErrForbidden, "not_org_owner", "only an organization owner can do this"))
	case errors.Is(err, database.ErrNotFound):
		writeError(w, r, newError(ErrNotFoundType, "member_not_found", "member not found"))
	default:
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to remove member"))
	}
}

type inviteResource struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	InvitedBy string `json:"invited_by"`
	ExpiresAt string `json:"expires_at"`
	CreatedAt string `json:"created_at"`
}

func (h *dashboardHandler) handleListInvites(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.orgAccess(w, r, true)
	if !ok {
		return
	}
	invs, err := h.db.ListPendingOrgInvitations(r.Context(), tenantID, h.now())
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to list invitations"))
		return
	}
	out := make([]inviteResource, 0, len(invs))
	for _, i := range invs {
		out = append(out, inviteResource{ID: i.ID, Email: i.RawEmail, InvitedBy: i.InvitedBy, ExpiresAt: i.ExpiresAt.UTC().Format(time.RFC3339), CreatedAt: i.CreatedAt.UTC().Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (h *dashboardHandler) handleRevokeInvite(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.orgAccess(w, r, true)
	if !ok {
		return
	}
	err := h.db.RevokeOrgInvitation(r.Context(), tenantID, r.PathValue("inviteId"), h.now())
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, database.ErrNotFound):
		writeError(w, r, newError(ErrNotFoundType, "invitation_not_found", "invitation not found"))
	default:
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to revoke invitation"))
	}
}

// orgAnalytics adapts an API-key analytics handler to the human-JWT org
// route: after the membership check it attaches the org as the request's
// tenant (withTenant, the single tenant-identity entry point) and delegates,
// so the analytics query/response code is shared, not duplicated (DEC-230).
func (h *dashboardHandler) orgAnalytics(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, tenantID, ok := h.orgAccess(w, r, false); ok {
			next(w, r.WithContext(withTenant(r.Context(), tenantID)))
		}
	}
}
