package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/idempotency"
	"github.com/Ferousco-dev/mailx/internal/observability"
	"github.com/Ferousco-dev/mailx/internal/ratelimit"
)

// Limiter is the slice of ratelimit.Store the API needs; tests substitute a
// failing one to prove the fail-closed behaviour.
type Limiter interface {
	Allow(ctx context.Context, buckets ...ratelimit.Bucket) (ratelimit.Decision, error)
	TenantRequestKey(tenantID string) string
	APIKeyGuardKey(tenantID, apiKeyRowID string) string
	TenantRecipientKey(tenantID string) string
}

// AbuseControls configures outbound abuse controls for the API. A nil
// *AbuseControls disables them (development and tests only; cmd/mailx refuses
// to run without them unless the operator opts out loudly).
//
// Failure policy (deliberate, documented in docs/design-v0.31.md): when the
// limiter store is unreachable, requests that send mail or change state FAIL
// CLOSED with 503 and Retry-After, because an unmetered outage would let a
// tenant send without limit. Read-only GETs FAIL OPEN, because they cannot send
// mail and refusing them would turn a Redis blip into a full API outage.
type AbuseControls struct {
	Limiter Limiter
	Policy  ratelimit.Policy
	Metrics *observability.Metrics
	Log     *slog.Logger
}

// unavailableRetryAfter is what a client is told when the limiter or a capacity
// check cannot answer: short, because such outages are usually brief.
const (
	unavailableRetryAfter = 5
	capacityRetryAfter    = 30
)

func (a *AbuseControls) log() *slog.Logger {
	if a == nil || a.Log == nil {
		return observability.Discard()
	}
	return a.Log
}

// requestLimitMiddleware charges the tenant bucket and the API key's own guard
// bucket atomically (all or nothing) for every authenticated /v1 request. The
// tenant bucket is charged by every key, so creating more API keys never raises
// a tenant's limit; the key bucket only stops one key from using the whole tenant
// allowance. Refusals never charge either bucket.
func requestLimitMiddleware(a *AbuseControls) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if a == nil || a.Limiter == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenantID := tenantFromContext(r.Context())
			keyID, _ := r.Context().Value(apiKeyIDKey).(string)
			p := a.Policy
			dec, err := a.Limiter.Allow(r.Context(),
				ratelimit.Bucket{Key: a.Limiter.TenantRequestKey(tenantID), Rate: p.TenantRequestRate, Burst: p.TenantRequestBurst, Cost: 1},
				ratelimit.Bucket{Key: a.Limiter.APIKeyGuardKey(tenantID, keyID), Rate: p.KeyRequestRate, Burst: p.KeyRequestBurst, Cost: 1},
			)
			switch {
			case err != nil:
				a.Metrics.AbuseDecision("request", "unavailable")
				a.log().Error("rate_limiter_unavailable", "route_class", routeClass(r))
				if r.Method == http.MethodGet || r.Method == http.MethodHead {
					next.ServeHTTP(w, r) // fail open for reads
					return
				}
				e := newError(ErrTemporarilyUnavailable, "rate_limiter_unavailable", "request limiting is temporarily unavailable; retry later")
				e.RetryAfter = unavailableRetryAfter
				writeError(w, r, e)
			case dec.Impossible:
				a.Metrics.AbuseDecision("request", "impossible")
				writeError(w, r, newError(ErrInternal, "internal_error", "request limit is misconfigured"))
			case !dec.Allowed:
				a.Metrics.AbuseDecision("request", "limited")
				code, msg := "tenant_rate_limited", "this account has sent too many requests; retry after the interval in Retry-After"
				if dec.Denied == 1 {
					code, msg = "api_key_rate_limited", "this API key has sent too many requests; retry after the interval in Retry-After"
				}
				e := newError(ErrRateLimited, code, msg)
				e.RetryAfter = ratelimit.RetryAfterSeconds(dec.RetryAfter)
				writeError(w, r, e)
			default:
				a.Metrics.AbuseDecision("request", "allowed")
				next.ServeHTTP(w, r)
			}
		})
	}
}

func routeClass(r *http.Request) string {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return "read"
	}
	return "write"
}

// admitSend applies the send-only controls after the request has passed every
// validation and (when an Idempotency-Key was supplied) owns its idempotency
// claim. Order is cheapest and least side-effecting first: the durable per-tenant
// queue cap, the global dispatch backpressure, and only then the recipient bucket,
// so a refusal by an earlier check never spends recipient tokens. It returns nil to
// proceed. On any refusal the caller must release the idempotency claim.
func (h *emailHandler) admitSend(ctx context.Context, tenantID string, deliverable int) *apiError {
	a := h.abuse
	if a == nil {
		return nil
	}
	p := a.Policy

	queued, err := h.db.CountTenantQueued(ctx, tenantID, p.TenantMaxQueuedMessages)
	if err != nil {
		return newError(ErrInternal, "internal_error", "failed to check the account's queue")
	}
	if queued >= p.TenantMaxQueuedMessages {
		a.Metrics.AbuseDecision("tenant_queue", "limited")
		e := newError(ErrRateLimited, "tenant_queue_full", "this account has too many undelivered messages; retry after they drain")
		e.RetryAfter = capacityRetryAfter
		return e
	}
	a.Metrics.AbuseDecision("tenant_queue", "allowed")

	pending, err := h.db.CountPendingOutbox(ctx, p.MaxPendingDispatch)
	if err != nil {
		return newError(ErrInternal, "internal_error", "failed to check system capacity")
	}
	if pending >= p.MaxPendingDispatch {
		a.Metrics.AbuseDecision("backpressure", "limited")
		e := newError(ErrTemporarilyUnavailable, "system_busy", "MailX is temporarily at capacity; retry later")
		e.RetryAfter = capacityRetryAfter
		return e
	}
	a.Metrics.AbuseDecision("backpressure", "allowed")

	dec, err := a.Limiter.Allow(ctx, ratelimit.Bucket{
		Key: a.Limiter.TenantRecipientKey(tenantID), Rate: p.TenantRecipientRate, Burst: p.TenantRecipientBurst, Cost: deliverable,
	})
	switch {
	case err != nil:
		a.Metrics.AbuseDecision("recipient", "unavailable")
		a.log().Error("rate_limiter_unavailable", "route_class", "send")
		e := newError(ErrTemporarilyUnavailable, "rate_limiter_unavailable", "sending limits are temporarily unavailable; retry later")
		e.RetryAfter = unavailableRetryAfter
		return e
	case dec.Impossible:
		a.Metrics.AbuseDecision("recipient", "impossible")
		return newError(ErrValidation, "too_many_recipients", "this message has more deliverable recipients than the account's sending burst allows")
	case !dec.Allowed:
		a.Metrics.AbuseDecision("recipient", "limited")
		e := newError(ErrRateLimited, "recipient_rate_limited", "this account is sending to recipients faster than its limit; retry after the interval in Retry-After")
		e.RetryAfter = ratelimit.RetryAfterSeconds(dec.RetryAfter)
		return e
	}
	a.Metrics.AbuseDecision("recipient", "allowed")
	return nil
}

// releaseClaim gives back an idempotency claim this request took but did not use,
// so a request refused by an abuse control does not consume its key: the client
// retries the same key after Retry-After. It uses a fresh short context because
// the request context may already be canceled, and a failure only means the
// claim expires by its staleness window instead.
func (h *emailHandler) releaseClaim(tenantID string, c *database.IdempotencyCompletion) {
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 2*time.Second)
	defer cancel()
	if err := h.db.ReleaseIdempotencyClaim(ctx, tenantID, idempotency.OperationEmailsCreate, c.IdempotencyKey, c.Fingerprint); err != nil {
		h.abuse.log().Warn("idempotency_claim_release_failed")
	}
}
