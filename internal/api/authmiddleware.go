package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/database"
)

// authService is the narrow seam authenticateMiddleware depends on —
// *auth.Service satisfies it in production; tests can substitute a fake
// without a real PostgreSQL connection.
type authService interface {
	Authenticate(ctx context.Context, raw string) (database.APIKey, error)
}

// maxCredentialLen bounds the Authorization header value before any
// parsing work — an attacker sending megabytes of garbage should fail
// fast, not walk the full string first.
const maxCredentialLen = 512

// authenticateMiddleware is v0.19's replacement for v0.18's
// devTenantMiddleware: it is now the ONLY place a request's tenant
// identity is determined (still via context.go's withTenant — this
// function is the sole caller from here on) and additionally attaches
// the authenticated key's scopes for requireScope to check. Every
// failure mode below produces the SAME external 401 response
// (authentication_error/invalid_api_key) — see the package doc on why
// external callers must never be able to distinguish "no such key" from
// "wrong secret" from "revoked" from "expired": that distinction is
// exactly what would let an attacker enumerate valid key identifiers.
func authenticateMiddleware(svc authService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := extractBearerToken(r)
			if err != nil {
				writeError(w, r, newError(ErrAuthentication, "invalid_api_key", "missing or malformed Authorization header"))
				return
			}
			record, err := svc.Authenticate(r.Context(), raw)
			if err != nil {
				if errors.Is(err, auth.ErrUnavailable) {
					// Fail closed: an unreachable source of truth must
					// never be treated as "request is authenticated".
					writeError(w, r, newError(ErrTemporarilyUnavailable, "authentication_unavailable", "authentication is temporarily unavailable"))
					return
				}
				// ErrUnknownKey, ErrWrongSecret, ErrRevoked, ErrExpired all
				// collapse to the same generic response — see doc above.
				writeError(w, r, newError(ErrAuthentication, "invalid_api_key", "invalid API key"))
				return
			}
			if m := metaFromContext(r.Context()); m != nil {
				m.tenantID = record.TenantID
			}
			ctx := withTenant(r.Context(), record.TenantID)
			ctx = withAuth(ctx, record.Scopes, record.ID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractBearerToken applies the strict parsing the spec calls out:
// exactly one Authorization header, exactly the "Bearer " scheme, a
// non-empty token bounded to a sane length before any further work.
func extractBearerToken(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", errMissingAuth
	}
	const prefix = "Bearer "
	v := values[0]
	if len(v) <= len(prefix) || !strings.HasPrefix(v, prefix) {
		return "", errMissingAuth
	}
	token := strings.TrimSpace(v[len(prefix):])
	if token == "" || len(token) > maxCredentialLen {
		return "", errMissingAuth
	}
	return token, nil
}

var errMissingAuth = errors.New("api: missing or malformed authorization")

// requireScope wraps a single route (not the whole /v1 subtree — routes
// need different scopes) and enforces authorization AFTER authentication
// has already run. A valid, authenticated key lacking the scope gets 403,
// distinct from every 401 case above — see the package doc's 401-vs-403
// split.
func requireScope(scope auth.Scope) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !hasScope(r.Context(), string(scope)) {
				writeError(w, r, newError(ErrForbidden, "insufficient_scope", "this API key does not have the required scope"))
				return
			}
			next.ServeHTTP(w, r)
		}
	}
}
