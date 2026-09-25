package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Auto-renewal policy (DEC-237). Timeline for a period ending at E:
// reminder at E-72h; charge attempts no earlier than E-48h, at most
// RenewalMaxAttempts, spaced >= RenewalRetrySpacing (so E-48h, E-36h, E-24h
// on an hourly ticker), all before E. No charge is ever made unless an
// auto-renew reminder for THIS period was sent >= RenewalMinNotice earlier.
// If every attempt fails, the tenant lapses at E through the unchanged
// DowngradeLapsedPlans path (retention pinned, DEC-227).
const (
	ReminderLead          = 72 * time.Hour
	RenewalChargeWindow   = 48 * time.Hour
	RenewalRetrySpacing   = 12 * time.Hour
	RenewalMaxAttempts    = 3
	RenewalMinNotice      = 24 * time.Hour
	RenewalReconcileAfter = time.Hour // a pending attempt older than this is verified with Paystack
)

// ChargeOutcome classifies a server-initiated charge (DEC-238).
type ChargeOutcome int

const (
	// ChargeUnknown: MailX cannot tell whether money moved (network error,
	// timeout, 5xx, undecodable body, or a non-final Paystack status). The
	// attempt MUST stay pending and be reconciled with VerifyTransaction;
	// it must never be retried under a new reference.
	ChargeUnknown ChargeOutcome = iota
	// ChargeSucceeded: Paystack reports status "success" for the reference.
	ChargeSucceeded
	// ChargeFailed: Paystack definitively did not take money (declined,
	// rejected request, abandoned, reversed, or reference never created).
	ChargeFailed
)

// ChargeResult is what a charge or verify call established. Reason is
// Paystack's human-readable gateway response/message (e.g. "Insufficient
// Funds"); it never contains card data. Amount/Currency are what Paystack
// reports, for the caller to check before applying.
type ChargeResult struct {
	Outcome  ChargeOutcome
	Reason   string
	Amount   int64
	Currency string
}

type txData struct {
	Status          string `json:"status"`
	Reference       string `json:"reference"`
	Amount          int64  `json:"amount"`
	Currency        string `json:"currency"`
	GatewayResponse string `json:"gateway_response"`
}

type txEnvelope struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    txData `json:"data"`
}

func classify(d txData) ChargeOutcome {
	switch strings.ToLower(d.Status) {
	case "success":
		return ChargeSucceeded
	case "failed", "abandoned", "reversed":
		return ChargeFailed
	default: // "pending", "ongoing", "processing", "queued", "send_otp", ...
		return ChargeUnknown
	}
}

// ChargeAuthorization calls POST /transaction/charge_authorization for plan's
// server-side price, under the caller-chosen reference (Paystack rejects a
// duplicate reference, a second line of defense against double charging).
// The authorization code is never logged or included in any error.
func (p *Paystack) ChargeAuthorization(ctx context.Context, email, authorizationCode string, plan Plan, reference string, meta Metadata) (ChargeResult, error) {
	if plan.PriceUSDCents <= 0 {
		return ChargeResult{Outcome: ChargeFailed, Reason: "plan is not purchasable"}, fmt.Errorf("billing: plan %q is not purchasable", plan.ID)
	}
	raw, err := json.Marshal(map[string]any{
		"email":              email,
		"amount":             plan.PriceUSDCents,
		"currency":           Currency,
		"authorization_code": authorizationCode,
		"reference":          reference,
		"metadata":           meta,
	})
	if err != nil {
		return ChargeResult{Outcome: ChargeFailed, Reason: "could not build request"}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/transaction/charge_authorization", bytes.NewReader(raw))
	if err != nil {
		return ChargeResult{Outcome: ChargeFailed, Reason: "could not build request"}, err
	}
	req.Header.Set("Content-Type", "application/json")
	return p.doTx(req, "charge_authorization")
}

// VerifyTransaction calls GET /transaction/verify/:reference, used to settle a
// charge whose outcome was ChargeUnknown. A reference Paystack does not know
// (HTTP 400/404 with status false) is ChargeFailed: no money moved.
func (p *Paystack) VerifyTransaction(ctx context.Context, reference string) (ChargeResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/transaction/verify/"+url.PathEscape(reference), nil)
	if err != nil {
		return ChargeResult{Outcome: ChargeUnknown}, err
	}
	return p.doTx(req, "verify")
}

func (p *Paystack) doTx(req *http.Request, op string) (ChargeResult, error) {
	req.Header.Set("Authorization", "Bearer "+p.secretKey)
	resp, err := p.http.Do(req)
	if err != nil {
		// Includes timeouts: the request may have reached Paystack.
		return ChargeResult{Outcome: ChargeUnknown}, fmt.Errorf("billing: paystack %s: %w", op, redactURLErr(err))
	}
	defer resp.Body.Close()
	var env txEnvelope
	decErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env)
	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return ChargeResult{Outcome: ChargeUnknown}, fmt.Errorf("billing: paystack %s: HTTP %d", op, resp.StatusCode)
	case decErr != nil:
		return ChargeResult{Outcome: ChargeUnknown}, fmt.Errorf("billing: paystack %s: decode (HTTP %d): %w", op, resp.StatusCode, decErr)
	case resp.StatusCode >= 400 || !env.Status:
		// Paystack rejected the request itself (bad/revoked authorization,
		// unknown reference, validation error): no transaction was charged.
		return ChargeResult{Outcome: ChargeFailed, Reason: env.Message}, nil
	}
	r := ChargeResult{Outcome: classify(env.Data), Reason: env.Data.GatewayResponse, Amount: env.Data.Amount, Currency: env.Data.Currency}
	if r.Reason == "" {
		r.Reason = env.Message
	}
	return r, nil
}

// redactURLErr keeps transport errors from echoing request URLs (verify URLs
// carry only the reference, but keep errors uniform and short).
func redactURLErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

// RenewalReference is the deterministic Paystack reference for one renewal
// attempt. It is unique per (tenant, period end, attempt) and has a prefix
// Paystack-generated checkout references never carry. Only characters
// Paystack accepts in a reference are used (alphanumerics and '-').
func RenewalReference(tenantID string, periodEndUnixMicro int64, attempt int) string {
	return fmt.Sprintf("mailx-renew-%s-%d-%d", tenantID, periodEndUnixMicro, attempt)
}
