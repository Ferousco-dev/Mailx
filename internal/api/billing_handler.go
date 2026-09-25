package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/billing"
	"github.com/Ferousco-dev/mailx/internal/database"
)

// BillingConfig enables /v1/billing/* (v0.47 phase 2, Paystack). Nil — the
// self-hosted default when MAILX_PAYSTACK_SECRET_KEY is unset — leaves the
// routes unregistered. Plan ENFORCEMENT is switched separately and solely by
// database.DB.EnablePlanEnforcement (DEC-221), which cmd/mailx turns on
// exactly when it builds a non-nil BillingConfig.
type BillingConfig struct {
	Paystack    *billing.Paystack
	CallbackURL string
}

// planPeriod is how long one successful charge keeps a paid plan active.
const planPeriod = 30 * 24 * time.Hour

// maxWebhookBody bounds the Paystack webhook body read.
const maxWebhookBody = 1 << 20

type billingHandler struct {
	db  *database.DB
	cfg *BillingConfig
	now func() time.Time
	log *slog.Logger
}

// planLimitAPIError maps a database.ErrPlanLimit to the API error shape;
// nil for any other error. status 429 is used for volume limits (same shape
// as the existing abuse controls), 403 for capability/cap limits.
func planLimitAPIError(err error, volume bool) *apiError {
	if !errors.Is(err, database.ErrPlanLimit) {
		return nil
	}
	msg := err.Error()
	if i := strings.Index(msg, database.ErrPlanLimit.Error()); i >= 0 {
		msg = msg[i:] // drop internal wrapping prefixes
	}
	msg += "; upgrade your plan to continue"
	if volume {
		e := newError(ErrRateLimited, "daily_send_limit_reached", msg)
		e.RetryAfter = secondsUntilUTCMidnight(time.Now().UTC())
		return e
	}
	return newError(ErrForbidden, "plan_limit_reached", msg)
}

func secondsUntilUTCMidnight(now time.Time) int {
	next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	s := int(next.Sub(now).Seconds())
	if s < 1 {
		s = 1
	}
	return s
}

type checkoutRequest struct {
	TenantID string `json:"tenant_id"`
	Plan     string `json:"plan"`
}

// handleCheckout starts a Paystack checkout for an org owner.
func (h *billingHandler) handleCheckout(w http.ResponseWriter, r *http.Request) {
	humanID, ok := humanIDFromContext(r.Context())
	if !ok {
		writeError(w, r, newError(ErrAuthentication, "invalid_access_token", "missing or invalid access token"))
		return
	}
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req checkoutRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "invalid_json", "request body is not valid JSON"))
		return
	}
	if req.TenantID == "" {
		writeError(w, r, newError(ErrValidation, "invalid_tenant", "tenant_id is required"))
		return
	}
	if !billing.IsPaid(req.Plan) {
		writeError(w, r, newError(ErrValidation, "invalid_plan", `plan must be "plus" or "pro"`))
		return
	}
	isOwner, err := h.db.IsTenantOwner(r.Context(), req.TenantID, humanID)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to check organization ownership"))
		return
	}
	if !isOwner {
		writeError(w, r, newError(ErrForbidden, "not_org_owner", "only an organization owner can change the plan"))
		return
	}
	owner, err := h.db.GetHuman(r.Context(), humanID)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load account"))
		return
	}
	res, err := h.cfg.Paystack.InitializeTransaction(r.Context(), owner.Email, billing.PlanFor(req.Plan),
		billing.Metadata{TenantID: req.TenantID, Plan: req.Plan}, h.cfg.CallbackURL)
	if err != nil {
		h.log.Error("billing_checkout_failed", "error", err.Error())
		writeError(w, r, newError(ErrTemporarilyUnavailable, "payment_provider_unavailable", "the payment provider could not start checkout; retry later"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"authorization_url": res.AuthorizationURL, "reference": res.Reference})
}

type subscriptionResponse struct {
	TenantID         string  `json:"tenant_id"`
	Plan             string  `json:"plan"`
	Status           string  `json:"status"`
	CurrentPeriodEnd *string `json:"current_period_end"`
}

// handleSubscription returns an org's plan to any of its members.
func (h *billingHandler) handleSubscription(w http.ResponseWriter, r *http.Request) {
	humanID, ok := humanIDFromContext(r.Context())
	if !ok {
		writeError(w, r, newError(ErrAuthentication, "invalid_access_token", "missing or invalid access token"))
		return
	}
	tenantID := r.URL.Query().Get("tenant_id")
	if tenantID == "" {
		writeError(w, r, newError(ErrValidation, "invalid_tenant", "tenant_id query parameter is required"))
		return
	}
	member, err := h.db.IsTenantMember(r.Context(), tenantID, humanID)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to check organization membership"))
		return
	}
	if !member {
		// Same answer for "no such org" and "not yours": no enumeration.
		writeError(w, r, newError(ErrNotFoundType, "organization_not_found", "organization not found"))
		return
	}
	tp, err := h.db.GetTenantPlan(r.Context(), tenantID)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load plan"))
		return
	}
	resp := subscriptionResponse{TenantID: tenantID, Plan: tp.Plan, Status: tp.Status}
	if tp.CurrentPeriodEnd != nil {
		s := tp.CurrentPeriodEnd.UTC().Format(time.RFC3339)
		resp.CurrentPeriodEnd = &s
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleWebhook is Paystack's public callback. Security boundary: nothing is
// parsed or applied until x-paystack-signature verifies (HMAC-SHA512 of the
// raw body with the secret key); failures are 401. Replays are neutralized by
// billing_payments' primary key on the transaction reference. Anything
// Paystack need not retry (unknown events, signed-but-unusable payloads,
// replays) is answered 200; only our own storage failures are 5xx.
func (h *billingHandler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		writeError(w, r, newError(ErrPayloadTooLarge, "body_too_large", "webhook body is too large"))
		return
	}
	if err := h.cfg.Paystack.VerifySignature(raw, r.Header.Get("x-paystack-signature")); err != nil {
		h.log.Warn("billing_webhook_bad_signature")
		writeError(w, r, newError(ErrAuthentication, "invalid_signature", "webhook signature is invalid"))
		return
	}
	ev, err := billing.ParseEvent(raw)
	if err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_webhook", "webhook body is malformed"))
		return
	}
	ack := func(result string) { writeJSON(w, http.StatusOK, map[string]string{"status": result}) }
	if ev.Event != "charge.success" {
		h.log.Info("billing_webhook_ignored", "event", ev.Event)
		ack("ignored")
		return
	}
	d := ev.Data
	plan := billing.PlanFor(d.Metadata.Plan)
	switch {
	case d.Reference == "", d.Metadata.TenantID == "", !billing.IsPaid(d.Metadata.Plan):
		h.log.Warn("billing_webhook_unusable", "reason", "missing reference/tenant/plan")
		ack("ignored")
		return
	case d.Status != "success", !strings.EqualFold(d.Currency, billing.Currency), d.Amount < plan.PriceUSDCents:
		h.log.Warn("billing_webhook_unusable", "reason", "status/currency/amount mismatch", "reference", d.Reference)
		ack("ignored")
		return
	}
	err = h.db.ApplyPlanPayment(r.Context(), database.Payment{
		Reference: d.Reference, TenantID: d.Metadata.TenantID, Plan: plan.ID,
		Amount: d.Amount, Currency: strings.ToUpper(d.Currency), CustomerCode: d.Customer.CustomerCode,
	}, planPeriod)
	switch {
	case errors.Is(err, database.ErrPaymentAlreadyApplied):
		h.log.Info("billing_webhook_replay", "reference", d.Reference)
		ack("already_applied")
	case errors.Is(err, database.ErrNotFound):
		h.log.Warn("billing_webhook_unknown_tenant", "reference", d.Reference)
		ack("ignored")
	case err != nil:
		h.log.Error("billing_webhook_apply_failed", "error", err.Error())
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to apply payment"))
	default:
		h.log.Info("billing_plan_applied", "tenant_id", d.Metadata.TenantID, "plan", plan.ID)
		ack("applied")
	}
}
