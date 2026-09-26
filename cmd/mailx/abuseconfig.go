package main

import (
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/api"
	"github.com/Ferousco-dev/mailx/internal/ratelimit"
	"github.com/Ferousco-dev/mailx/internal/worker"
	"github.com/redis/go-redis/v9"
)

// Outbound abuse controls (v0.31). Every variable is optional; the defaults are
// ratelimit.DefaultPolicy(). A value that is set but invalid FAILS STARTUP naming
// the variable: a typo must never silently weaken a safety limit (contrast the
// lenient envInt used for tuning knobs, which falls back to its default).
//
//	MAILX_ABUSE_CONTROLS                    on (default) | off — "off" disables every
//	                                        control below and logs a loud warning
//	MAILX_LIMIT_TENANT_RPS / _BURST         API requests per tenant (50 / 100)
//	MAILX_LIMIT_KEY_RPS / _BURST            API requests per API key, <= tenant (25 / 50)
//	MAILX_LIMIT_RECIPIENTS_PER_SEC / _BURST deliverable recipients per tenant (20 / 500)
//	MAILX_LIMIT_MAX_RECIPIENTS              recipients per message, <= burst (50)
//	MAILX_LIMIT_TENANT_MAX_QUEUED           undelivered messages per tenant (10000)
//	MAILX_LIMIT_MAX_PENDING_DISPATCH        system-wide undispatched messages (100000)
//	MAILX_LIMIT_TENANT_CONCURRENCY          concurrent SMTP attempts per tenant (16)
//	MAILX_LIMIT_DESTINATION_CONCURRENCY     concurrent SMTP attempts per destination domain (16)
//	MAILX_LIMIT_PERMIT_TTL                  crash-recovery bound for a permit (10m, 10s..24h)
//	MAILX_RETRY_JITTER_PERCENT              retry-time spread, 0-50 (10)
type abuseRuntime struct {
	Enabled bool
	Policy  ratelimit.Policy
	Store   *ratelimit.Store
	rdb     *redis.Client
}

func (a *abuseRuntime) Close() {
	if a != nil && a.rdb != nil {
		_ = a.rdb.Close()
	}
}

// loadAbusePolicy reads and validates the configuration. It takes the lookup as a
// parameter so tests and the fuzzer never touch the process environment.
func loadAbusePolicy(get func(string) string) (policy ratelimit.Policy, enabled bool, err error) {
	switch strings.ToLower(strings.TrimSpace(get("MAILX_ABUSE_CONTROLS"))) {
	case "", "on":
		enabled = true
	case "off":
		return ratelimit.DefaultPolicy(), false, nil
	default:
		return policy, false, fmt.Errorf("MAILX_ABUSE_CONTROLS must be \"on\" or \"off\", got %q", get("MAILX_ABUSE_CONTROLS"))
	}
	policy = ratelimit.DefaultPolicy()
	var errs []error
	f := func(key string, dst *float64) {
		if raw := strings.TrimSpace(get(key)); raw != "" {
			v, e := strconv.ParseFloat(raw, 64)
			if e != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				errs = append(errs, fmt.Errorf("%s must be a finite number", key))
				return
			}
			*dst = v
		}
	}
	n := func(key string, dst *int) {
		if raw := strings.TrimSpace(get(key)); raw != "" {
			v, e := strconv.Atoi(raw)
			if e != nil {
				errs = append(errs, fmt.Errorf("%s must be a whole number", key))
				return
			}
			*dst = v
		}
	}
	f("MAILX_LIMIT_TENANT_RPS", &policy.TenantRequestRate)
	n("MAILX_LIMIT_TENANT_BURST", &policy.TenantRequestBurst)
	f("MAILX_LIMIT_KEY_RPS", &policy.KeyRequestRate)
	n("MAILX_LIMIT_KEY_BURST", &policy.KeyRequestBurst)
	f("MAILX_LIMIT_RECIPIENTS_PER_SEC", &policy.TenantRecipientRate)
	n("MAILX_LIMIT_RECIPIENTS_BURST", &policy.TenantRecipientBurst)
	n("MAILX_LIMIT_MAX_RECIPIENTS", &policy.MaxRecipientsPerMessage)
	n("MAILX_LIMIT_TENANT_MAX_QUEUED", &policy.TenantMaxQueuedMessages)
	n("MAILX_LIMIT_MAX_PENDING_DISPATCH", &policy.MaxPendingDispatch)
	n("MAILX_LIMIT_TENANT_CONCURRENCY", &policy.TenantDeliveryConcurrency)
	n("MAILX_LIMIT_DESTINATION_CONCURRENCY", &policy.DestinationDeliveryConcurrency)
	f("MAILX_LIMIT_AUTH_IP_RPS", &policy.AuthIPRate)
	n("MAILX_LIMIT_AUTH_IP_BURST", &policy.AuthIPBurst)
	f("MAILX_LIMIT_PASSWORD_RESET_IP_RPS", &policy.PasswordResetIPRate)
	n("MAILX_LIMIT_PASSWORD_RESET_IP_BURST", &policy.PasswordResetIPBurst)
	f("MAILX_LIMIT_MFA_VERIFY_IP_RPS", &policy.MFAVerifyIPRate)
	n("MAILX_LIMIT_MFA_VERIFY_IP_BURST", &policy.MFAVerifyIPBurst)
	f("MAILX_LIMIT_ORG_INVITE_RPS", &policy.OrgInviteRate)
	n("MAILX_LIMIT_ORG_INVITE_BURST", &policy.OrgInviteBurst)
	f("MAILX_LIMIT_APIKEY_MINT_RPS", &policy.APIKeyMintRate)
	n("MAILX_LIMIT_APIKEY_MINT_BURST", &policy.APIKeyMintBurst)
	n("MAILX_RETRY_JITTER_PERCENT", &policy.RetryJitterPercent)
	if raw := strings.TrimSpace(get("MAILX_LIMIT_PERMIT_TTL")); raw != "" {
		d, e := time.ParseDuration(raw)
		if e != nil {
			errs = append(errs, fmt.Errorf("MAILX_LIMIT_PERMIT_TTL must be a duration such as 10m"))
		} else {
			policy.PermitTTL = d
		}
	}
	if len(errs) > 0 {
		return policy, false, errs[0]
	}
	if err := policy.Validate(); err != nil {
		return policy, false, fmt.Errorf("abuse controls: %w", err)
	}
	return policy, true, nil
}

// openAbuseControls loads the configuration and, when enabled, opens the limiter's
// own Redis client (the queue keeps its own; the limiter's keys live under a
// separate "mailx:limit" prefix).
func openAbuseControls(o obs) (*abuseRuntime, error) {
	policy, enabled, err := loadAbusePolicy(os.Getenv)
	if err != nil {
		return nil, err
	}
	if !enabled {
		o.log.Warn("abuse_controls_disabled", "hint", "MAILX_ABUSE_CONTROLS=off: no request, recipient, queue or concurrency limits apply; use only for local development")
		return &abuseRuntime{Policy: policy}, nil
	}
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	// No startup ping: the queue's own startup and readiness already decide whether
	// Redis is reachable, and a limiter outage is handled per request (fail closed).
	o.log.Info("abuse_controls_configured",
		"tenant_rps", policy.TenantRequestRate, "tenant_burst", policy.TenantRequestBurst,
		"key_rps", policy.KeyRequestRate, "key_burst", policy.KeyRequestBurst,
		"recipients_per_sec", policy.TenantRecipientRate, "recipients_burst", policy.TenantRecipientBurst,
		"max_recipients", policy.MaxRecipientsPerMessage, "tenant_max_queued", policy.TenantMaxQueuedMessages,
		"max_pending_dispatch", policy.MaxPendingDispatch, "tenant_concurrency", policy.TenantDeliveryConcurrency,
		"destination_concurrency", policy.DestinationDeliveryConcurrency, "permit_ttl", policy.PermitTTL.String(),
		"retry_jitter_percent", policy.RetryJitterPercent)
	return &abuseRuntime{Enabled: true, Policy: policy, Store: ratelimit.NewStore(rdb, ""), rdb: rdb}, nil
}

// apiControls returns the API's controls, or nil when disabled.
func (a *abuseRuntime) apiControls(o obs) *api.AbuseControls {
	if a == nil || !a.Enabled {
		return nil
	}
	return &api.AbuseControls{Limiter: a.Store, Policy: a.Policy, Metrics: o.metrics, Log: o.log, TrustedProxyCIDRs: trustedProxyCIDRs(o)}
}

// trustedProxyCIDRs parses MAILX_TRUSTED_PROXY_CIDRS (comma-separated
// CIDRs, e.g. "10.0.0.0/8,172.16.0.0/12") — see AbuseControls.TrustedProxyCIDRs's
// doc for what this enables. Empty/unset means no proxy is trusted (the
// safe default for a direct, no-reverse-proxy deployment). An invalid
// entry is logged and skipped rather than failing startup — a typo here
// should degrade to "no trusted proxies" (RemoteAddr-keyed, safe), not
// crash the server.
func trustedProxyCIDRs(o obs) []*net.IPNet {
	raw := os.Getenv("MAILX_TRUSTED_PROXY_CIDRS")
	if raw == "" {
		return nil
	}
	var out []*net.IPNet
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, cidr, err := net.ParseCIDR(part)
		if err != nil {
			o.log.Warn("invalid_trusted_proxy_cidr", "value", part, "error", err.Error())
			continue
		}
		out = append(out, cidr)
	}
	return out
}

// workerOptions returns the worker's permit option, or nothing when disabled.
func (a *abuseRuntime) workerOptions() []worker.Option {
	if a == nil || !a.Enabled {
		return nil
	}
	return []worker.Option{worker.WithPermits(a.Store, worker.PermitPolicy{
		TenantLimit: a.Policy.TenantDeliveryConcurrency, DestinationLimit: a.Policy.DestinationDeliveryConcurrency,
		TTL: a.Policy.PermitTTL, JitterPercent: a.Policy.RetryJitterPercent,
	})}
}

// retryJitter is the backoff jitter, zero when abuse controls are off.
func (a *abuseRuntime) retryJitter() int {
	if a == nil || !a.Enabled {
		return 0
	}
	return a.Policy.RetryJitterPercent
}
