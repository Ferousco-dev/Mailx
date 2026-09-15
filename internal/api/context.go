package api

import "context"

// ctxKey is a private type so context values here can never collide with
// keys set by other packages (including future v0.19 auth middleware).
type ctxKey int

const (
	tenantKey ctxKey = iota
	requestIDKey
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
