package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultPaystackBaseURL is Paystack's API root.
const DefaultPaystackBaseURL = "https://api.paystack.co"

// Currency is the only currency MailX bills in (operator decision).
const Currency = "USD"

// ErrInvalidSignature means a webhook body's x-paystack-signature did not
// verify. Callers must answer 401 and apply nothing.
var ErrInvalidSignature = errors.New("billing: invalid paystack signature")

// Paystack is a minimal Paystack client: Initialize Transaction and webhook
// signature verification. The secret key never leaves this struct.
type Paystack struct {
	secretKey string
	baseURL   string
	http      *http.Client
}

// NewPaystack returns a client for secretKey. baseURL "" selects the real
// Paystack API; tests point it at an httptest server.
func NewPaystack(secretKey, baseURL string) (*Paystack, error) {
	if strings.TrimSpace(secretKey) == "" {
		return nil, errors.New("billing: paystack secret key is empty")
	}
	if baseURL == "" {
		baseURL = DefaultPaystackBaseURL
	}
	return &Paystack{secretKey: secretKey, baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 15 * time.Second}}, nil
}

// String never prints the secret key.
func (p *Paystack) String() string { return "billing.Paystack{baseURL:" + p.baseURL + "}" }

// Metadata is what MailX attaches to every transaction and reads back from
// the verified webhook.
type Metadata struct {
	TenantID string `json:"tenant_id"`
	Plan     string `json:"plan"`
}

// UnmarshalJSON tolerates metadata that isn't a JSON object (Paystack sends
// non-object metadata, e.g. 0 or "", for transactions this checkout never
// created - a payment page or another integration on the same account).
// Such a transaction decodes to an empty Metadata rather than failing the
// whole webhook parse: the handler already ignores an empty TenantID
// (CodeRabbit, PR #24) - a signed event MailX cannot recognize must still
// be acknowledged 200, not answered 400 (which makes Paystack retry
// forever for an event that will never become recognizable).
func (m *Metadata) UnmarshalJSON(b []byte) error {
	type plain Metadata
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		*m = Metadata{}
		return nil
	}
	*m = Metadata(p)
	return nil
}

// InitializeResult is the part of Paystack's response the frontend needs.
type InitializeResult struct {
	AuthorizationURL string
	Reference        string
}

// InitializeTransaction calls POST /transaction/initialize for plan's price
// in USD cents, returning the hosted checkout URL.
func (p *Paystack) InitializeTransaction(ctx context.Context, email string, plan Plan, meta Metadata, callbackURL string) (InitializeResult, error) {
	if plan.PriceUSDCents <= 0 {
		return InitializeResult{}, fmt.Errorf("billing: plan %q is not purchasable", plan.ID)
	}
	body := map[string]any{
		"email":    email,
		"amount":   plan.PriceUSDCents,
		"currency": Currency,
		"metadata": meta,
	}
	if callbackURL != "" {
		body["callback_url"] = callbackURL
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return InitializeResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/transaction/initialize", bytes.NewReader(raw))
	if err != nil {
		return InitializeResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.secretKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return InitializeResult{}, fmt.Errorf("billing: paystack initialize: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Status  bool   `json:"status"`
		Message string `json:"message"`
		Data    struct {
			AuthorizationURL string `json:"authorization_url"`
			Reference        string `json:"reference"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return InitializeResult{}, fmt.Errorf("billing: paystack initialize: decode (HTTP %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || !out.Status || out.Data.AuthorizationURL == "" {
		return InitializeResult{}, fmt.Errorf("billing: paystack initialize failed (HTTP %d): %s", resp.StatusCode, out.Message)
	}
	return InitializeResult{AuthorizationURL: out.Data.AuthorizationURL, Reference: out.Data.Reference}, nil
}

// VerifySignature checks Paystack's documented webhook signature:
// hex(HMAC-SHA512(secretKey, rawBody)) in the x-paystack-signature header,
// compared in constant time. An empty or non-hex header never verifies.
func (p *Paystack) VerifySignature(rawBody []byte, header string) error {
	got, err := hex.DecodeString(strings.TrimSpace(header))
	if err != nil || len(got) != sha512.Size {
		return ErrInvalidSignature
	}
	mac := hmac.New(sha512.New, []byte(p.secretKey))
	mac.Write(rawBody)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return ErrInvalidSignature
	}
	return nil
}

// Sign computes the signature Paystack would send for body. Exported for
// tests and local tooling; production code only verifies.
func (p *Paystack) Sign(body []byte) string {
	mac := hmac.New(sha512.New, []byte(p.secretKey))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Event is the subset of a Paystack webhook event MailX reads.
type Event struct {
	Event string `json:"event"`
	Data  struct {
		Reference string   `json:"reference"`
		Status    string   `json:"status"`
		Amount    int64    `json:"amount"`
		Currency  string   `json:"currency"`
		Metadata  Metadata `json:"metadata"`
		Customer  struct {
			CustomerCode string `json:"customer_code"`
			Email        string `json:"email"`
		} `json:"customer"`
		// Authorization is the card token Paystack returns on a successful
		// charge. AuthorizationCode is SENSITIVE (it can be charged again):
		// never log it; store it only encrypted (DEC-236).
		Authorization struct {
			AuthorizationCode string `json:"authorization_code"`
			Reusable          bool   `json:"reusable"`
		} `json:"authorization"`
	} `json:"data"`
}

// ParseEvent decodes a (verified) webhook body.
func ParseEvent(rawBody []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(rawBody, &e); err != nil {
		return Event{}, fmt.Errorf("billing: malformed webhook body: %w", err)
	}
	if e.Event == "" {
		return Event{}, errors.New("billing: webhook body has no event type")
	}
	return e, nil
}
