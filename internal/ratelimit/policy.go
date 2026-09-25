package ratelimit

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Policy is the operator's abuse-control configuration. It is a plain, explicit set
// of limits with no notion of plans, prices or free tiers: MailX Cloud plans can map
// onto it later, and self-hosters set it directly.
//
// Vocabulary (used consistently in code, config, API errors and docs):
//   - message: one POST /v1/emails accepted as a durable unit.
//   - recipient: one address across To/Cc/Bcc of a message.
//   - deliverable recipient: a recipient that is not suppressed (a suppressed
//     recipient is never delivered, so it consumes no delivery capacity).
//   - delivery attempt: one SMTP operation for a message.
type Policy struct {
	// Tenant request bucket: every authenticated /v1 request costs 1. The tenant is
	// the authoritative boundary; creating more API keys never adds capacity.
	TenantRequestRate  float64
	TenantRequestBurst int
	// API-key guard bucket: additionally limits one key so a compromised or looping
	// integration cannot spend the whole tenant allowance. Must not exceed the
	// tenant limit (a looser guard would give a false sense of containment).
	KeyRequestRate  float64
	KeyRequestBurst int
	// Recipient-volume bucket: a NEW message costs its number of deliverable
	// recipients. Idempotent replays and suppressed recipients cost nothing.
	TenantRecipientRate  float64
	TenantRecipientBurst int
	// Durable caps (PostgreSQL). TenantMaxQueuedMessages bounds a tenant's messages
	// still waiting for their first delivery outcome (429); MaxPendingDispatch bounds
	// the whole instance's undispatched outbox (503).
	TenantMaxQueuedMessages int
	MaxPendingDispatch      int
	// Worker concurrency caps: simultaneous SMTP attempts for one tenant, and for
	// one destination domain (a generic receiver-side courtesy, not provider logic).
	TenantDeliveryConcurrency      int
	DestinationDeliveryConcurrency int
	// PermitTTL bounds how long a crashed worker can hold a permit.
	PermitTTL time.Duration
	// RetryJitterPercent spreads retry times by up to +/- this percent, so messages
	// that failed together do not retry together.
	RetryJitterPercent int
	// MaxRecipientsPerMessage is validated against the recipient bucket's burst.
	MaxRecipientsPerMessage int
	// AuthIPRate/AuthIPBurst bound the human-auth surface (POST
	// /v1/auth/signup, /login, /refresh) per client IP. Unlike every other
	// bucket in this policy, these requests are UNAUTHENTICATED by
	// definition (there is no tenant/API key yet), so IP is the only
	// identity available to key on. Deliberately tighter than the tenant
	// request rate: this is a password-guessing/account-enumeration
	// surface, not ordinary API traffic.
	AuthIPRate  float64
	AuthIPBurst int
	// PasswordResetIPRate/PasswordResetIPBurst bound POST
	// /v1/auth/forgot-password and /v1/auth/reset-password per client IP,
	// separately from and much tighter than AuthIPRate: forgot-password
	// triggers a real outbound email send on every call (a shared burst
	// with login would let a caller email-bomb a victim's inbox by
	// spamming forgot-password far more cheaply than the login-guessing
	// budget was sized for), and reset-password carries the account's most
	// dangerous credential-change action.
	PasswordResetIPRate  float64
	PasswordResetIPBurst int
}

const (
	maxRate        = 1_000_000.0
	maxBurst       = 10_000_000
	maxConcurrency = 100_000
	maxCount       = 1_000_000_000
)

// DefaultPolicy returns generous defaults that protect a self-hosted instance
// without getting in the way of local development. They are operator defaults, not a
// product contract.
func DefaultPolicy() Policy {
	return Policy{
		TenantRequestRate: 50, TenantRequestBurst: 100,
		KeyRequestRate: 25, KeyRequestBurst: 50,
		TenantRecipientRate: 20, TenantRecipientBurst: 500,
		TenantMaxQueuedMessages: 10_000, MaxPendingDispatch: 100_000,
		TenantDeliveryConcurrency: 16, DestinationDeliveryConcurrency: 16,
		PermitTTL: 10 * time.Minute, RetryJitterPercent: 10,
		MaxRecipientsPerMessage: 50,
		AuthIPRate:              1, AuthIPBurst: 10,
		PasswordResetIPRate: 1.0 / 60, PasswordResetIPBurst: 3,
	}
}

// Validate rejects nonsensical or dangerous values instead of silently repairing
// them. Errors name the setting, never a secret.
func (p Policy) Validate() error {
	rate := func(name string, v float64) error {
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > maxRate {
			return fmt.Errorf("%s must be a number greater than 0 and at most %.0f", name, maxRate)
		}
		return nil
	}
	count := func(name string, v, max int) error {
		if v < 1 || v > max {
			return fmt.Errorf("%s must be between 1 and %d", name, max)
		}
		return nil
	}
	for _, e := range []error{
		rate("tenant request rate", p.TenantRequestRate), count("tenant request burst", p.TenantRequestBurst, maxBurst),
		rate("API key request rate", p.KeyRequestRate), count("API key request burst", p.KeyRequestBurst, maxBurst),
		rate("tenant recipient rate", p.TenantRecipientRate), count("tenant recipient burst", p.TenantRecipientBurst, maxBurst),
		count("tenant max queued messages", p.TenantMaxQueuedMessages, maxCount), count("max pending dispatch", p.MaxPendingDispatch, maxCount),
		count("tenant delivery concurrency", p.TenantDeliveryConcurrency, maxConcurrency),
		count("destination delivery concurrency", p.DestinationDeliveryConcurrency, maxConcurrency),
		count("max recipients per message", p.MaxRecipientsPerMessage, 1000),
		rate("auth IP rate", p.AuthIPRate), count("auth IP burst", p.AuthIPBurst, maxBurst),
		rate("password reset IP rate", p.PasswordResetIPRate), count("password reset IP burst", p.PasswordResetIPBurst, maxBurst),
	} {
		if e != nil {
			return e
		}
	}
	if p.KeyRequestRate > p.TenantRequestRate || p.KeyRequestBurst > p.TenantRequestBurst {
		return errors.New("the API key request limit must not exceed the tenant request limit")
	}
	if p.TenantRecipientBurst < p.MaxRecipientsPerMessage {
		return fmt.Errorf("tenant recipient burst (%d) must be at least the maximum recipients per message (%d), or a full message could never be accepted",
			p.TenantRecipientBurst, p.MaxRecipientsPerMessage)
	}
	if p.PermitTTL < 10*time.Second || p.PermitTTL > 24*time.Hour {
		return errors.New("permit TTL must be between 10s and 24h")
	}
	if p.RetryJitterPercent < 0 || p.RetryJitterPercent > 50 {
		return errors.New("retry jitter percent must be between 0 and 50")
	}
	return nil
}

// RetryAfterSeconds converts a wait into the whole seconds of a Retry-After header:
// rounded UP (never tell a client to retry too early) with a floor of 1 and a
// ceiling of one hour.
func RetryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	s := int64((d + time.Second - 1) / time.Second)
	if s < 1 {
		s = 1
	}
	if s > 3600 {
		s = 3600
	}
	return int(s)
}
