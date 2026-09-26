package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	stdmail "net/mail"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/humanauth"
)

// humanAuthHandler serves /v1/auth/* and /v1/orgs — human/browser session
// endpoints. Deliberately separate from every existing handler in this
// package: those are all authenticated (or not) via the API-key
// authService/requireScope path; this handler has its own JWT-based
// middleware (see humanAuthMiddleware below) and must never be reachable
// through requireScope, nor vice versa.
type humanAuthHandler struct {
	svc *humanauth.Service
}

type humanResource struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Email       string  `json:"email"`
	LastLoginAt *string `json:"last_login_at"`
}

type sessionResponse struct {
	Human        humanResource `json:"human"`
	AccessToken  string        `json:"access_token"`
	RefreshToken string        `json:"refresh_token"`
}

func sessionResponseFrom(s humanauth.Session) sessionResponse {
	var lastLogin *string
	if s.Human.LastLoginAt != nil {
		formatted := s.Human.LastLoginAt.Format(time.RFC3339)
		lastLogin = &formatted
	}
	return sessionResponse{
		Human:        humanResource{ID: s.Human.ID, Name: s.Human.Name, Email: s.Human.Email, LastLoginAt: lastLogin},
		AccessToken:  s.AccessToken,
		RefreshToken: s.RefreshToken,
	}
}

type signupRequest struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *humanAuthHandler) handleSignup(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req signupRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "invalid_json", "request body is not valid JSON"))
		return
	}
	session, err := h.svc.SignUp(r.Context(), req.Name, req.Email, req.Password)
	if err != nil {
		if errors.Is(err, humanauth.ErrEmailTaken) {
			writeError(w, r, newError(ErrConflictType, "email_taken", "an account with this email already exists"))
			return
		}
		writeError(w, r, newError(ErrValidation, "invalid_signup", err.Error()))
		return
	}
	writeJSON(w, http.StatusCreated, sessionResponseFrom(session))
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Remember bool   `json:"remember"`
}

func (h *humanAuthHandler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "invalid_json", "request body is not valid JSON"))
		return
	}
	session, err := h.svc.Login(r.Context(), req.Email, req.Password)
	if writeMFARequired(w, err) {
		return
	}
	if err != nil {
		// Login intentionally returns the SAME error for "no such email"
		// and "wrong password" — see humanauth.Service.Login's doc.
		writeError(w, r, newError(ErrAuthentication, "invalid_credentials", "invalid email or password"))
		return
	}
	writeJSON(w, http.StatusOK, sessionResponseFrom(session))
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *humanAuthHandler) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req refreshRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil || req.RefreshToken == "" {
		writeError(w, r, newError(ErrInvalidRequest, "invalid_json", "refresh_token is required"))
		return
	}
	session, err := h.svc.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		writeError(w, r, newError(ErrAuthentication, "invalid_refresh_token", "refresh token is invalid, expired, or revoked"))
		return
	}
	writeJSON(w, http.StatusOK, sessionResponseFrom(session))
}

func (h *humanAuthHandler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req refreshRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil || req.RefreshToken == "" {
		writeError(w, r, newError(ErrInvalidRequest, "invalid_json", "refresh_token is required"))
		return
	}
	if err := h.svc.Logout(r.Context(), req.RefreshToken); err != nil {
		writeError(w, r, newError(ErrInternal, "logout_failed", "could not log out"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type forgotPasswordRequest struct {
	Email string `json:"email"`
}

// handleForgotPassword ALWAYS responds with the same generic message,
// whatever humanauth.Service.ForgotPassword actually did internally
// (found-and-emailed, not-found, or even a mailer/DB failure) — the
// response must never let a caller distinguish "this email has an
// account" from "it doesn't", the same anti-enumeration posture Login
// already uses. A genuine internal error is logged, never surfaced.
func (h *humanAuthHandler) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req forgotPasswordRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil || req.Email == "" {
		writeError(w, r, newError(ErrInvalidRequest, "invalid_json", "email is required"))
		return
	}
	if err := h.svc.ForgotPassword(r.Context(), req.Email); err != nil {
		slog.Default().Error("forgot_password_failed", "error", err.Error())
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "if an account exists for that email, a password reset link has been sent"})
}

type resetPasswordRequest struct {
	Token           string `json:"token"`
	NewPassword     string `json:"new_password"`
	ConfirmPassword string `json:"confirm_password"`
}

func (h *humanAuthHandler) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req resetPasswordRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "invalid_json", "request body is not valid JSON"))
		return
	}
	if req.Token == "" || req.NewPassword == "" {
		writeError(w, r, newError(ErrValidation, "invalid_reset", "token and new_password are required"))
		return
	}
	// A client-side mismatch check is the primary UX guard; this is
	// defense in depth against a client bug, not the only check.
	if req.ConfirmPassword != "" && req.ConfirmPassword != req.NewPassword {
		writeError(w, r, newError(ErrValidation, "password_mismatch", "new_password and confirm_password do not match"))
		return
	}
	if err := h.svc.ResetPassword(r.Context(), req.Token, req.NewPassword); err != nil {
		if errors.Is(err, humanauth.ErrPasswordResetTokenInvalid) {
			writeError(w, r, newError(ErrValidation, "invalid_reset_token", "this reset link is invalid or has expired"))
			return
		}
		writeError(w, r, newError(ErrValidation, "invalid_reset", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "password updated; all other sessions have been signed out"})
}

type verifyEmailRequest struct {
	Token string `json:"token"`
}

// handleVerifyEmail is public: the link may be opened on a device with no
// session. Every token failure collapses to one generic error.
func (h *humanAuthHandler) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req verifyEmailRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil || req.Token == "" {
		writeError(w, r, newError(ErrInvalidRequest, "invalid_json", "token is required"))
		return
	}
	if err := h.svc.VerifyEmail(r.Context(), req.Token); err != nil {
		if errors.Is(err, humanauth.ErrEmailVerificationTokenInvalid) {
			writeError(w, r, newError(ErrValidation, "invalid_verification_token", "this verification link is invalid or has expired"))
			return
		}
		slog.Default().Error("verify_email_failed", "error", err.Error())
		writeError(w, r, newError(ErrInternal, "internal_error", "could not verify email"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "email verified"})
}

// handleResendVerification ALWAYS responds with the same generic message
// (unknown, already verified, sent, or internal failure) - same
// anti-enumeration posture as handleForgotPassword.
func (h *humanAuthHandler) handleResendVerification(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req forgotPasswordRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil || req.Email == "" {
		writeError(w, r, newError(ErrInvalidRequest, "invalid_json", "email is required"))
		return
	}
	if err := h.svc.ResendVerification(r.Context(), req.Email); err != nil {
		slog.Default().Error("resend_verification_failed", "error", err.Error())
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "if an unverified account exists for that email, a verification link has been sent"})
}

type createOrgRequest struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type orgResource struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

func orgResourceFrom(t database.Tenant) orgResource {
	return orgResource{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt}
}

func (h *humanAuthHandler) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	humanID, ok := humanIDFromContext(r.Context())
	if !ok {
		writeError(w, r, newError(ErrAuthentication, "invalid_access_token", "missing or invalid access token"))
		return
	}
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req createOrgRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "invalid_json", "request body is not valid JSON"))
		return
	}
	tenant, err := h.svc.CreateOrganization(r.Context(), humanID, req.Name, req.Slug)
	if err != nil {
		writeError(w, r, newError(ErrValidation, "invalid_organization", err.Error()))
		return
	}
	writeJSON(w, http.StatusCreated, orgResourceFrom(tenant))
}

func (h *humanAuthHandler) handleListOrgs(w http.ResponseWriter, r *http.Request) {
	humanID, ok := humanIDFromContext(r.Context())
	if !ok {
		writeError(w, r, newError(ErrAuthentication, "invalid_access_token", "missing or invalid access token"))
		return
	}
	tenants, err := h.svc.ListOrganizationsForHuman(r.Context(), humanID)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "list_organizations_failed", "could not list organizations"))
		return
	}
	out := make([]orgResource, 0, len(tenants))
	for _, t := range tenants {
		out = append(out, orgResourceFrom(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

type inviteRequest struct {
	Email string `json:"email"`
}

// handleCreateInvite sends an org invitation. Owner-only: the org ID comes
// from the path, the inviter's identity from the JWT (never trust a
// client-supplied "am I the owner" claim). humanauth.Service.InviteToOrganization
// itself re-checks tenant_members before doing anything, so this handler's
// job is only request parsing and error-shape translation.
func (h *humanAuthHandler) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	humanID, ok := humanIDFromContext(r.Context())
	if !ok {
		writeError(w, r, newError(ErrAuthentication, "invalid_access_token", "missing or invalid access token"))
		return
	}
	tenantID := r.PathValue("id")
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req inviteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil || req.Email == "" {
		writeError(w, r, newError(ErrInvalidRequest, "invalid_json", "email is required"))
		return
	}
	// Validate BEFORE it ever reaches the database: an address with, say,
	// internal whitespace would otherwise trip the invitation table's own
	// constraint and surface a raw PostgreSQL error to the caller instead
	// of a useful validation response (Greptile P2, PR #23).
	if _, err := stdmail.ParseAddress(req.Email); err != nil {
		writeError(w, r, newError(ErrValidation, "invalid_email", "email is not a valid address"))
		return
	}
	if err := h.svc.InviteToOrganization(r.Context(), humanID, tenantID, req.Email); err != nil {
		if errors.Is(err, humanauth.ErrNotOrgOwner) {
			writeError(w, r, newError(ErrForbidden, "not_org_owner", "only an organization owner can send invitations"))
			return
		}
		if aerr := planLimitAPIError(err, false); aerr != nil {
			writeError(w, r, aerr)
			return
		}
		slog.Default().Error("org_invite_failed", "error", err.Error())
		writeError(w, r, newError(ErrValidation, "invalid_invitation", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "invitation sent"})
}

type acceptInviteRequest struct {
	Token    string `json:"token"`
	Name     string `json:"name"`     // only used when the caller has no existing session
	Password string `json:"password"` // only used when the caller has no existing session
}

// handleAcceptInvite is the single combined accept endpoint (operator
// decision): a caller with a valid human access token accepts under their
// existing account (email must match the invite); a caller with no
// Authorization header must supply name+password and is signed up and
// joined atomically in one call, keyed by the invite token. This route is
// intentionally NOT behind humanAuthMiddleware — that middleware would
// reject every unauthenticated (no-account-yet) request before this
// handler ever saw it — so the bearer token here is read directly and
// treated as optional.
func (h *humanAuthHandler) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req acceptInviteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil || req.Token == "" {
		writeError(w, r, newError(ErrInvalidRequest, "invalid_json", "token is required"))
		return
	}
	var existingHumanID string
	if token, ok := extractBearerHumanToken(r); ok {
		claims, err := h.svc.VerifyAccessToken(token)
		if err != nil {
			writeError(w, r, newError(ErrAuthentication, "invalid_access_token", "access token is invalid or expired"))
			return
		}
		existingHumanID = claims.HumanID
	}
	result, err := h.svc.AcceptOrgInvitation(r.Context(), req.Token, existingHumanID, req.Name, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, database.ErrPlanLimit):
			writeError(w, r, planLimitAPIError(err, false))
		case errors.Is(err, humanauth.ErrOrgInvitationInvalid):
			writeError(w, r, newError(ErrValidation, "invalid_invitation_token", "this invitation is invalid or has expired"))
		case errors.Is(err, humanauth.ErrOrgInvitationEmailMismatch):
			writeError(w, r, newError(ErrForbidden, "invitation_email_mismatch", "this invitation was sent to a different email address"))
		case errors.Is(err, humanauth.ErrEmailTaken):
			writeError(w, r, newError(ErrConflictType, "email_taken", "an account with this email already exists; log in and try again"))
		default:
			writeError(w, r, newError(ErrValidation, "invalid_invitation", err.Error()))
		}
		return
	}
	resp := map[string]any{"organization": orgResourceFrom(result.Tenant)}
	if result.Session != nil {
		resp["session"] = sessionResponseFrom(*result.Session)
	}
	writeJSON(w, http.StatusOK, resp)
}

// extractBearerHumanToken parses "Bearer <token>" the same way
// extractBearerToken does for API keys, kept separate so the two
// credential formats never share validation code paths.
func extractBearerHumanToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

// humanAuthMiddleware verifies a human JWT access token and attaches the
// human's ID to the request context. It is the human-session counterpart
// to authenticateMiddleware (API keys) and is never combined with it —
// each request is authenticated as EITHER a tenant (via API key) OR a
// human (via JWT), never both by the same middleware function.
func humanAuthMiddleware(svc *humanauth.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := extractBearerHumanToken(r)
			if !ok {
				writeError(w, r, newError(ErrAuthentication, "invalid_access_token", "missing or malformed Authorization header"))
				return
			}
			claims, err := svc.VerifyAccessToken(token)
			if err != nil {
				writeError(w, r, newError(ErrAuthentication, "invalid_access_token", "access token is invalid or expired"))
				return
			}
			next.ServeHTTP(w, withHumanID(r, claims.HumanID))
		})
	}
}
