package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Ferousco-dev/mailx/internal/humanauth"
)

// OAuth sign-in and TOTP MFA endpoints (v0.47 phase 3b, DEC-231..DEC-234).

func registerMFAAndOAuthRoutes(mux *http.ServeMux, ha *humanAuthHandler, authenticated func(http.Handler) http.Handler, abuse *AbuseControls) {
	mux.Handle("GET /v1/auth/oauth/{provider}/start", chain(http.HandlerFunc(ha.handleOAuthStart), authIPLimitMiddleware(abuse)))
	mux.Handle("GET /v1/auth/oauth/{provider}/callback", chain(http.HandlerFunc(ha.handleOAuthCallback), authIPLimitMiddleware(abuse)))
	mux.Handle("POST /v1/auth/mfa/verify", chain(http.HandlerFunc(ha.handleMFAVerify), mfaVerifyIPLimitMiddleware(abuse)))
	mux.Handle("POST /v1/auth/mfa/enroll", authenticated(http.HandlerFunc(ha.handleMFAEnroll)))
	mux.Handle("POST /v1/auth/mfa/confirm", authenticated(chain(http.HandlerFunc(ha.handleMFAConfirm), mfaVerifyIPLimitMiddleware(abuse))))
	mux.Handle("POST /v1/auth/mfa/disable", authenticated(chain(http.HandlerFunc(ha.handleMFADisable), authIPLimitMiddleware(abuse))))
}

// writeMFARequired writes the intermediate MFA state if err is
// *humanauth.MFARequiredError, reporting whether it did.
func writeMFARequired(w http.ResponseWriter, err error) bool {
	var mfa *humanauth.MFARequiredError
	if !errors.As(err, &mfa) {
		return false
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mfa_required": true,
		"mfa_token":    mfa.ChallengeToken,
		"expires_at":   mfa.ExpiresAt.UTC().Format(time.RFC3339),
	})
	return true
}

func decodeHumanJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(dst); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "invalid_json", "request body is not valid JSON"))
		return false
	}
	return true
}

func (h *humanAuthHandler) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	u, err := h.svc.StartOAuth(r.Context(), r.PathValue("provider"))
	if errors.Is(err, humanauth.ErrOAuthProviderNotConfigured) {
		writeError(w, r, newError(ErrNotFoundType, "oauth_provider_not_configured", "this sign-in provider is not configured on this server"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "oauth_start_failed", "could not start sign-in"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"authorization_url": u})
}

func (h *humanAuthHandler) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := q.Get("code")
	if q.Get("error") != "" {
		code = "" // consent denied: still consume the state, then report a provider error
	}
	session, err := h.svc.CompleteOAuth(r.Context(), r.PathValue("provider"), code, q.Get("state"))
	if writeMFARequired(w, err) {
		return
	}
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, sessionResponseFrom(session))
	case errors.Is(err, humanauth.ErrOAuthProviderNotConfigured):
		writeError(w, r, newError(ErrNotFoundType, "oauth_provider_not_configured", "this sign-in provider is not configured on this server"))
	case errors.Is(err, humanauth.ErrOAuthStateInvalid):
		writeError(w, r, newError(ErrAuthentication, "invalid_oauth_state", "sign-in state is missing, expired, or already used; start again"))
	case errors.Is(err, humanauth.ErrOAuthProvider):
		writeError(w, r, newError(ErrAuthentication, "oauth_provider_error", "the sign-in provider did not complete authentication"))
	case errors.Is(err, humanauth.ErrOAuthAccountRequiresPasswordLogin):
		writeError(w, r, newError(ErrConflictType, "oauth_account_requires_password_login", "an account with this email already has a password; log in with it first to link this sign-in method"))
	default:
		writeError(w, r, newError(ErrInternal, "oauth_callback_failed", "could not complete sign-in"))
	}
}

type mfaCodeRequest struct {
	MFAToken string `json:"mfa_token"`
	Code     string `json:"code"`
	Password string `json:"password"`
}

func (h *humanAuthHandler) handleMFAVerify(w http.ResponseWriter, r *http.Request) {
	var req mfaCodeRequest
	if !decodeHumanJSON(w, r, &req) {
		return
	}
	if req.MFAToken == "" || req.Code == "" {
		writeError(w, r, newError(ErrInvalidRequest, "invalid_json", "mfa_token and code are required"))
		return
	}
	session, err := h.svc.VerifyMFA(r.Context(), req.MFAToken, req.Code)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, sessionResponseFrom(session))
	case errors.Is(err, humanauth.ErrMFAChallengeInvalid):
		writeError(w, r, newError(ErrAuthentication, "invalid_mfa", "MFA challenge is invalid or expired, or the code is incorrect"))
	case errors.Is(err, humanauth.ErrMFANotConfigured):
		writeError(w, r, newError(ErrTemporarilyUnavailable, "mfa_not_configured", "authenticator codes cannot be checked on this server right now; use a backup code"))
	default:
		writeError(w, r, newError(ErrInternal, "mfa_verify_failed", "could not verify MFA"))
	}
}

func (h *humanAuthHandler) handleMFAEnroll(w http.ResponseWriter, r *http.Request) {
	humanID, ok := humanIDFromContext(r.Context())
	if !ok {
		writeError(w, r, newError(ErrAuthentication, "invalid_access_token", "missing human session"))
		return
	}
	enr, err := h.svc.EnrollMFA(r.Context(), humanID)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"secret": enr.Secret, "otpauth_uri": enr.OTPAuthURI, "backup_codes": enr.BackupCodes})
	case errors.Is(err, humanauth.ErrMFANotConfigured):
		writeError(w, r, newError(ErrNotFoundType, "mfa_not_configured", "MFA is not configured on this server"))
	case errors.Is(err, humanauth.ErrMFAAlreadyEnabled):
		writeError(w, r, newError(ErrConflictType, "mfa_already_enabled", "MFA is already enabled; disable it first"))
	default:
		writeError(w, r, newError(ErrInternal, "mfa_enroll_failed", "could not start MFA enrollment"))
	}
}

func (h *humanAuthHandler) handleMFAConfirm(w http.ResponseWriter, r *http.Request) {
	humanID, ok := humanIDFromContext(r.Context())
	if !ok {
		writeError(w, r, newError(ErrAuthentication, "invalid_access_token", "missing human session"))
		return
	}
	var req mfaCodeRequest
	if !decodeHumanJSON(w, r, &req) {
		return
	}
	err := h.svc.ConfirmMFA(r.Context(), humanID, req.Code)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"mfa_enabled": true})
	case errors.Is(err, humanauth.ErrMFANotConfigured):
		writeError(w, r, newError(ErrNotFoundType, "mfa_not_configured", "MFA is not configured on this server"))
	case errors.Is(err, humanauth.ErrMFACodeInvalid):
		writeError(w, r, newError(ErrValidation, "invalid_mfa_code", "the code is incorrect"))
	case errors.Is(err, humanauth.ErrMFANotPending), errors.Is(err, humanauth.ErrMFAAlreadyEnabled):
		writeError(w, r, newError(ErrConflictType, "mfa_not_pending", "no pending MFA enrollment; call /v1/auth/mfa/enroll first"))
	default:
		writeError(w, r, newError(ErrInternal, "mfa_confirm_failed", "could not confirm MFA"))
	}
}

func (h *humanAuthHandler) handleMFADisable(w http.ResponseWriter, r *http.Request) {
	humanID, ok := humanIDFromContext(r.Context())
	if !ok {
		writeError(w, r, newError(ErrAuthentication, "invalid_access_token", "missing human session"))
		return
	}
	var req mfaCodeRequest
	if !decodeHumanJSON(w, r, &req) {
		return
	}
	err := h.svc.DisableMFA(r.Context(), humanID, req.Password)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"mfa_enabled": false})
	case errors.Is(err, humanauth.ErrInvalidCredentials):
		writeError(w, r, newError(ErrAuthentication, "invalid_credentials", "password is incorrect"))
	default:
		writeError(w, r, newError(ErrInternal, "mfa_disable_failed", "could not disable MFA"))
	}
}
