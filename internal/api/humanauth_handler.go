package api

import (
	"encoding/json"
	"errors"
	"net/http"
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
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type sessionResponse struct {
	Human        humanResource `json:"human"`
	AccessToken  string        `json:"access_token"`
	RefreshToken string        `json:"refresh_token"`
}

func sessionResponseFrom(s humanauth.Session) sessionResponse {
	return sessionResponse{
		Human:        humanResource{ID: s.Human.ID, Name: s.Human.Name, Email: s.Human.Email},
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
