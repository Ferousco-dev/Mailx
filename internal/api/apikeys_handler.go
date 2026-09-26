package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/ratelimit"
)

// apiKeyManager is the subset of *auth.Service the org API-key routes need.
// These routes call the SAME Create/Rotate/Revoke/List the CLI
// (cmd/mailx/apikeys.go) uses, so key generation, hashing, and storage exist
// in exactly one place (DEC-245).
type apiKeyManager interface {
	Create(ctx context.Context, tenantID, name string, scopes []string, ttl *time.Duration) (auth.Generated, database.APIKey, error)
	Rotate(ctx context.Context, keyID string, grace time.Duration) (auth.Generated, database.APIKey, error)
	Revoke(ctx context.Context, keyID string) error
	List(ctx context.Context, tenantID string) ([]database.APIKey, error)
}

// orgAPIKeyHandler serves /v1/orgs/{id}/api-keys: human-JWT authenticated,
// authorized with dashboardHandler.orgAccess (member for list, owner for
// create/rotate/revoke — DEC-228 posture). The org is the tenant (DEC-205).
type orgAPIKeyHandler struct {
	dash *dashboardHandler
	keys apiKeyManager
}

const maxAPIKeyNameLen = 200

type apiKeyResource struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Scopes     []string `json:"scopes"`
	Status     string   `json:"status"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt *string  `json:"last_used_at"`
	ExpiresAt  *string  `json:"expires_at"`
	RevokedAt  *string  `json:"revoked_at"`
}

type createdAPIKeyResource struct {
	apiKeyResource
	// Key is the raw credential. It is returned only by create/rotate and
	// is never stored or retrievable again.
	Key string `json:"key"`
}

func fmtTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func toAPIKeyResource(k database.APIKey, now time.Time) apiKeyResource {
	status := "active"
	switch {
	case k.RevokedAt != nil:
		status = "revoked"
	case k.ExpiresAt != nil && !k.ExpiresAt.After(now):
		status = "expired"
	}
	scopes := k.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	return apiKeyResource{
		ID: k.KeyID, Name: k.Name, Scopes: scopes, Status: status,
		CreatedAt:  k.CreatedAt.UTC().Format(time.RFC3339),
		LastUsedAt: fmtTimePtr(k.LastUsedAt), ExpiresAt: fmtTimePtr(k.ExpiresAt), RevokedAt: fmtTimePtr(k.RevokedAt),
	}
}

type createAPIKeyBody struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

func (h *orgAPIKeyHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.dash.orgAccess(w, r, true)
	if !ok {
		return
	}
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var b createAPIKeyBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&b); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "invalid_json", "request body is not valid JSON"))
		return
	}
	name := strings.TrimSpace(b.Name)
	if name == "" || len(name) > maxAPIKeyNameLen {
		writeError(w, r, newError(ErrValidation, "invalid_name", "name must be 1-200 characters"))
		return
	}
	if len(b.Scopes) == 0 {
		writeError(w, r, newError(ErrValidation, "invalid_scopes", "at least one scope is required"))
		return
	}
	seen := make(map[string]bool, len(b.Scopes))
	for _, s := range b.Scopes {
		if !auth.ValidScope(s) {
			writeError(w, r, newError(ErrValidation, "invalid_scopes", "unknown scope: "+s))
			return
		}
		if seen[s] {
			writeError(w, r, newError(ErrValidation, "invalid_scopes", "duplicate scope: "+s))
			return
		}
		seen[s] = true
	}
	gen, rec, err := h.keys.Create(r.Context(), tenantID, name, b.Scopes, nil)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to create api key"))
		return
	}
	writeJSON(w, http.StatusCreated, createdAPIKeyResource{apiKeyResource: toAPIKeyResource(rec, h.dash.now()), Key: gen.Raw})
}

func (h *orgAPIKeyHandler) handleList(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.dash.orgAccess(w, r, false)
	if !ok {
		return
	}
	keys, err := h.keys.List(r.Context(), tenantID)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to list api keys"))
		return
	}
	now := h.dash.now()
	out := make([]apiKeyResource, 0, len(keys))
	for _, k := range keys {
		out = append(out, toAPIKeyResource(k, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// orgKey loads {keyId} and confirms it belongs to the {id} org. A key of
// another tenant is indistinguishable from a nonexistent one (404).
func (h *orgAPIKeyHandler) orgKey(w http.ResponseWriter, r *http.Request, tenantID string) (database.APIKey, bool) {
	k, err := h.dash.db.GetAPIKeyByKeyID(r.Context(), r.PathValue("keyId"))
	if errors.Is(err, database.ErrNotFound) || (err == nil && k.TenantID != tenantID) {
		writeError(w, r, newError(ErrNotFoundType, "api_key_not_found", "api key not found"))
		return database.APIKey{}, false
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load api key"))
		return database.APIKey{}, false
	}
	return k, true
}

func (h *orgAPIKeyHandler) handleRotate(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.dash.orgAccess(w, r, true)
	if !ok {
		return
	}
	k, ok := h.orgKey(w, r, tenantID)
	if !ok {
		return
	}
	now := h.dash.now()
	if k.RevokedAt != nil || (k.ExpiresAt != nil && !k.ExpiresAt.After(now)) {
		writeError(w, r, newError(ErrConflictType, "api_key_not_active", "only an active api key can be rotated; create a new one instead"))
		return
	}
	// Grace 0: the old key stops authenticating immediately (DEC-247).
	gen, rec, err := h.keys.Rotate(r.Context(), k.KeyID, 0)
	if err != nil {
		if errors.Is(err, database.ErrConflict) || errors.Is(err, database.ErrNotFound) {
			// Lost a race with a concurrent revoke/rotate (re-checked under
			// the row lock in database.RotateAPIKey).
			writeError(w, r, newError(ErrConflictType, "api_key_not_active", "only an active api key can be rotated; create a new one instead"))
			return
		}
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to rotate api key"))
		return
	}
	writeJSON(w, http.StatusOK, createdAPIKeyResource{apiKeyResource: toAPIKeyResource(rec, now), Key: gen.Raw})
}

// handleRevoke matches the CLI's revoke-api-key semantics exactly: revoking
// an already-revoked key reports not-found (database.RevokeAPIKey only
// updates rows WHERE revoked_at IS NULL) and changes nothing (DEC-247).
func (h *orgAPIKeyHandler) handleRevoke(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.dash.orgAccess(w, r, true)
	if !ok {
		return
	}
	k, ok := h.orgKey(w, r, tenantID)
	if !ok {
		return
	}
	err := h.keys.Revoke(r.Context(), k.KeyID)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, database.ErrNotFound):
		writeError(w, r, newError(ErrNotFoundType, "api_key_not_found", "api key not found or already revoked"))
	default:
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to revoke api key"))
	}
}

// apiKeyMintLimitMiddleware bounds POST create/rotate per human, mirroring
// orgInviteLimitMiddleware (DEC-247): an authenticated, owner-gated,
// occasional mutation keyed on the verified human ID rather than IP.
func apiKeyMintLimitMiddleware(a *AbuseControls) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if a == nil || a.Limiter == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			humanID, _ := humanIDFromContext(r.Context())
			p := a.Policy
			dec, err := a.Limiter.Allow(r.Context(),
				ratelimit.Bucket{Key: "org:apikey:human:" + humanID, Rate: p.APIKeyMintRate, Burst: p.APIKeyMintBurst, Cost: 1},
			)
			switch {
			case err != nil:
				a.Metrics.AbuseDecision("api_key_mint", "unavailable")
				a.log().Error("rate_limiter_unavailable", "route_class", "api_key_mint")
				e := newError(ErrTemporarilyUnavailable, "rate_limiter_unavailable", "request limiting is temporarily unavailable; retry later")
				e.RetryAfter = unavailableRetryAfter
				writeError(w, r, e)
			case dec.Impossible:
				a.Metrics.AbuseDecision("api_key_mint", "impossible")
				writeError(w, r, newError(ErrInternal, "internal_error", "request limit is misconfigured"))
			case !dec.Allowed:
				a.Metrics.AbuseDecision("api_key_mint", "limited")
				e := newError(ErrRateLimited, "api_key_rate_limited", "too many api keys created or rotated; retry after the interval in Retry-After")
				e.RetryAfter = ratelimit.RetryAfterSeconds(dec.RetryAfter)
				writeError(w, r, e)
			default:
				a.Metrics.AbuseDecision("api_key_mint", "allowed")
				next.ServeHTTP(w, r)
			}
		})
	}
}
