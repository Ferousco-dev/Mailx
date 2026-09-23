package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Ferousco-dev/mailx/internal/idempotency"
	"github.com/Ferousco-dev/mailx/internal/ratelimit"
)

// MaxBatchSize bounds POST /v1/emails/batch. Chosen as a round number
// consistent with this package's existing bounds (defaultMaxRecipients=50
// per message, maxLimit=100 for pagination) — large enough to be useful,
// small enough that one HTTP request/response body and one admitSend loop
// stay cheap and predictable. Not tied to any per-tenant plan/tier concept,
// since none exists yet in this codebase.
const MaxBatchSize = 100

// batchProcessingBudget bounds the WHOLE per-item loop below writeTimeout
// (15s): pollIdempotencyCompletion can block up to idempotencyPollTimeout
// (5s) per item when its key is already claimed in-progress elsewhere, so
// a batch of unlucky items processed strictly sequentially could otherwise
// take minutes and have its connection closed mid-response, silently
// leaving later items unprocessed despite this endpoint's one-result-per-
// item contract (PR review finding). Once the budget is spent, every
// remaining item gets an explicit batch_processing_timeout result instead
// of being silently dropped or hung on.
const batchProcessingBudget = 10 * time.Second

// batchSendItem is one email within a batch request: the exact same shape
// POST /v1/emails accepts, plus an optional per-item idempotency key.
// There is no batch-level Idempotency-Key header — that HTTP mechanism is
// inherently single-value per request, and items are independent messages
// (unlike v0.36 Broadcasts, which fan one template out to one audience),
// so each item gets its own optional key instead, reusing the EXACT same
// idempotency.Fingerprint/ClaimIdempotencyKey machinery POST /v1/emails
// already uses (via acceptOne) rather than inventing a second mechanism.
type batchSendItem struct {
	sendEmailRequest
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type batchSendRequest struct {
	Emails []batchSendItem `json:"emails"`
}

// batchErrorBody mirrors errorBody's per-error fields (type/code/message)
// without the top-level request_id, which is a whole-HTTP-request concept,
// not a per-item one.
type batchErrorBody struct {
	Type    ErrorType `json:"type"`
	Code    string    `json:"code"`
	Message string    `json:"message"`
}

// batchItemResult reports one item's outcome, at the same index as its
// request item, so a client can always correlate a result back to what it
// submitted. Exactly one of Email/Error is set.
type batchItemResult struct {
	Index int             `json:"index"`
	Email *email          `json:"email,omitempty"`
	Error *batchErrorBody `json:"error,omitempty"`
}

type batchSendResponse struct {
	Data     []batchItemResult `json:"data"`
	Accepted int               `json:"accepted"`
	Rejected int               `json:"rejected"`
}

// handleSendBatch implements POST /v1/emails/batch: N independent emails
// (distinct recipients/content each) accepted in one call. It is NOT
// v0.36 Broadcasts (one template fanned out to one audience) — every item
// here goes through the EXACT same per-message acceptance pipeline
// (acceptOne) POST /v1/emails uses: same validation, same idempotency
// contract, same DKIM/domain authorization, same suppression check, same
// abuse controls, same durable InsertMessage. There is no batch-level
// transaction and no batch-level abuse-control bypass: each item is
// admitted (or refused) independently, exactly as if it had been sent as
// its own POST /v1/emails call — a large batch cannot buy a sender more
// throughput than the same N individual requests would have gotten.
//
// PARTIAL FAILURE, not all-or-nothing: one item's rejection never blocks
// its siblings. Every item — good or bad — gets exactly one result in the
// response, in request order, so a client can correlate index -> outcome
// without any ambiguity.
func (h *emailHandler) handleSendBatch(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}

	var req batchSendRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_json", "request body is not valid JSON: "+err.Error()))
		return
	}
	if len(req.Emails) == 0 {
		writeError(w, r, newError(ErrValidation, "empty_batch", "emails must contain at least one item"))
		return
	}
	if len(req.Emails) > MaxBatchSize {
		writeError(w, r, newError(ErrValidation, "batch_too_large", fmt.Sprintf("a batch may contain at most %d items, got %d", MaxBatchSize, len(req.Emails))))
		return
	}
	for i, item := range req.Emails {
		if item.IdempotencyKey != "" {
			if err := idempotency.ValidateKey(item.IdempotencyKey); err != nil {
				writeError(w, r, newError(ErrInvalidRequest, "invalid_idempotency_key", fmt.Sprintf("item %d: %s", i, err.Error())))
				return
			}
		}
	}

	tenantID := tenantFromContext(r.Context())

	// requestLimitMiddleware already charged Cost:1 for this ONE HTTP
	// request before routing ever reached this handler — that is correct
	// for a single-item request but, unadjusted, lets a 100-item batch
	// spend one token to perform up to 100x the CPU/DB work (DKIM RSA
	// signing, verified-sender lookup, template render, suppression
	// query) of a normal send (PR review finding). Charge the SAME
	// tenant/key request buckets the remaining (N-1) tokens a batch this
	// size actually costs, BEFORE any of that expensive per-item work
	// runs — a refusal here rejects the whole batch (429) with zero items
	// processed, exactly like any other abuse-control refusal in this
	// package.
	if extra := len(req.Emails) - 1; extra > 0 && h.abuse != nil && h.abuse.Limiter != nil {
		keyID, _ := r.Context().Value(apiKeyIDKey).(string)
		p := h.abuse.Policy
		dec, err := h.abuse.Limiter.Allow(r.Context(),
			ratelimit.Bucket{Key: h.abuse.Limiter.TenantRequestKey(tenantID), Rate: p.TenantRequestRate, Burst: p.TenantRequestBurst, Cost: extra},
			ratelimit.Bucket{Key: h.abuse.Limiter.APIKeyGuardKey(tenantID, keyID), Rate: p.KeyRequestRate, Burst: p.KeyRequestBurst, Cost: extra},
		)
		switch {
		case err != nil:
			h.abuse.Metrics.AbuseDecision("request", "unavailable")
			e := newError(ErrTemporarilyUnavailable, "rate_limiter_unavailable", "request limiting is temporarily unavailable; retry later")
			e.RetryAfter = unavailableRetryAfter
			writeError(w, r, e)
			return
		case dec.Impossible:
			h.abuse.Metrics.AbuseDecision("request", "impossible")
			writeError(w, r, newError(ErrValidation, "batch_too_large", "this batch is larger than the account's request burst allows"))
			return
		case !dec.Allowed:
			h.abuse.Metrics.AbuseDecision("request", "limited")
			code, msg := "tenant_rate_limited", "this account has sent too many requests; retry after the interval in Retry-After"
			if dec.Denied == 1 {
				code, msg = "api_key_rate_limited", "this API key has sent too many requests; retry after the interval in Retry-After"
			}
			e := newError(ErrRateLimited, code, msg)
			e.RetryAfter = ratelimit.RetryAfterSeconds(dec.RetryAfter)
			writeError(w, r, e)
			return
		default:
			h.abuse.Metrics.AbuseDecision("request", "allowed")
		}
	}

	now := h.now()
	recipientLimit := h.recipientLimit()

	// Bounded below writeTimeout — see batchProcessingBudget's doc. ctx,
	// not r.Context(), is what acceptOne receives below, so a deadline hit
	// mid-item still lets that item's own DB work unwind cleanly via the
	// normal context-cancellation paths already used everywhere else in
	// this codebase, rather than the request's OWN context (which stays
	// live until the handler returns) hiding the timeout entirely.
	budget := h.batchBudget
	if budget <= 0 {
		budget = batchProcessingBudget
	}
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()

	results := make([]batchItemResult, len(req.Emails))
	var accepted, rejected int
	for i, item := range req.Emails {
		if ctx.Err() != nil {
			results[i] = batchItemResult{Index: i, Error: &batchErrorBody{
				Type: ErrTemporarilyUnavailable, Code: "batch_processing_timeout",
				Message: "this item was not processed before the batch's processing budget was reached; resubmit it (and any other timed-out items) on their own",
			}}
			rejected++
			continue
		}
		scheduledAt, verr := item.sendEmailRequest.validate(now, recipientLimit)
		if verr != nil {
			results[i] = batchItemResult{Index: i, Error: &batchErrorBody{Type: verr.Type, Code: verr.Code, Message: verr.Message}}
			rejected++
			continue
		}
		resp, _, apiErr := h.acceptOne(ctx, tenantID, item.sendEmailRequest, item.IdempotencyKey, now, scheduledAt)
		if apiErr != nil {
			results[i] = batchItemResult{Index: i, Error: &batchErrorBody{Type: apiErr.Type, Code: apiErr.Code, Message: apiErr.Message}}
			rejected++
			continue
		}
		results[i] = batchItemResult{Index: i, Email: &resp}
		accepted++
	}

	writeJSON(w, http.StatusAccepted, batchSendResponse{Data: results, Accepted: accepted, Rejected: rejected})
}
