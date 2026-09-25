package api

import (
	"context"
	"net/http"
)

// ctxKey is a private type so context values here can never collide with
// keys set by other packages (including future v0.19 auth middleware).
type ctxKey int

const (
	tenantKey ctxKey = iota
	requestIDKey
	scopesKey
	apiKeyIDKey
	humanIDKey
)

// withTenant is the ONLY place a request's tenant identity is attached to
// its context. v0.19 replaces just the middleware that calls this (deriving
// the tenant from an API key instead of the v0.18 development mechanism —
// see middleware.go's devTenant) — handlers never change.
func withTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantKey, tenantID)
}

// tenantFromContext panics if called on a request that did not go through
// the tenant middleware — every route under /v1 must, so this is a
// programmer error, not a runtime condition to handle gracefully.
func tenantFromContext(ctx context.Context) string {
	id, ok := ctx.Value(tenantKey).(string)
	if !ok || id == "" {
		panic("api: request has no tenant in context; the tenant middleware was not applied to this route")
	}
	return id
}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

func requestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// withAuth attaches the authenticated request's scopes and the api_keys
// row id (for logging only — never the key's public key_id or secret) to
// tenant+request context in one call, set by authenticateMiddleware.
func withAuth(ctx context.Context, scopes []string, apiKeyRowID string) context.Context {
	ctx = context.WithValue(ctx, scopesKey, scopes)
	return context.WithValue(ctx, apiKeyIDKey, apiKeyRowID)
}

func scopesFromContext(ctx context.Context) []string {
	scopes, _ := ctx.Value(scopesKey).([]string)
	return scopes
}

// withHumanID attaches a human-session (JWT) identity to the context —
// set only by humanAuthMiddleware, never by authenticateMiddleware (API
// keys). The two identity kinds are read by disjoint sets of handlers.
func withHumanID(r *http.Request, humanID string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), humanIDKey, humanID))
}

func humanIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(humanIDKey).(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

func hasScope(ctx context.Context, scope string) bool {
	for _, s := range scopesFromContext(ctx) {
		if s == scope {
			return true
		}
	}
	return false
}
