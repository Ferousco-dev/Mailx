package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/retry"
)

// Permits bounds concurrent SMTP work per tenant and per destination domain so one
// tenant (or one slow remote domain) cannot occupy every worker. Implemented by
// ratelimit.Store; workers hold a permit only for the duration of one attempt.
type Permits interface {
	Acquire(ctx context.Context, key string, limit int, ttl time.Duration, holder string) (bool, error)
	Release(ctx context.Context, key, holder string) error
	TenantPermitKey(tenantID string) string
	DestinationPermitKey(domain string) string
}

// TenantLookup is the optional OutcomeStore capability that names a message's
// tenant. Stores without it (tests) get destination permits only.
type TenantLookup interface {
	MessageTenant(ctx context.Context, messageID string) (string, error)
}

// PermitPolicy configures WithPermits.
type PermitPolicy struct {
	TenantLimit      int
	DestinationLimit int
	// TTL is the crash-recovery bound: a permit held by a worker that died is
	// reclaimed after it. It must exceed the longest legitimate attempt.
	TTL time.Duration
	// JitterPercent spreads deferrals (0-50) so deferred jobs do not all return
	// at the same instant.
	JitterPercent int
	// Deferral is how long a job waits after a refused permit (default 5s).
	Deferral time.Duration
}

// WithPermits enables per-tenant and per-destination concurrency limits.
func WithPermits(perm Permits, pol PermitPolicy) Option {
	return func(p *Pool) {
		p.permits = perm
		p.permitPolicy = pol
	}
}

// Deferral delays, deliberately short: a permit is freed as soon as any in-flight
// attempt for that tenant or domain finishes.
const (
	permitDeferral        = 5 * time.Second // default PermitPolicy.Deferral
	permitUnknownDeferral = 15 * time.Second
	permitOpTimeout       = 2 * time.Second
)

// acquirePermits takes the tenant and destination permits for one attempt. It
// returns done (release both; call once) and ok=true when the caller may proceed.
// When ok=false the job has ALREADY been released back to the queue with a
// jittered delay: no delivery attempt is recorded, the retry counters do not move,
// and no SMTP happens. Deferring is not failing: a message cannot fail or bounce
// because its tenant was busy.
//
// Fail-closed: an unknown permit state (Redis or the tenant lookup failing) defers
// the job rather than sending unmetered.
func (p *Pool) acquirePermits(ctx context.Context, c queue.Claim, domain string) (done func(), ok bool) {
	noop := func() {}
	if p.permits == nil {
		return noop, true
	}
	holder := c.Job.ID
	var held []string
	release := func() {
		// Fresh context: the pool's own context may already be canceled on shutdown,
		// and a leaked permit would only be reclaimed by its TTL.
		rctx, cancel := context.WithTimeout(context.Background(), permitOpTimeout)
		defer cancel()
		for _, k := range held {
			if err := p.permits.Release(rctx, k, holder); err != nil {
				p.log.Warn("permit_release_failed", "job_id", c.Job.ID)
			}
		}
	}

	if lookup, has := p.outcomes.(TenantLookup); has {
		tenant, err := lookup.MessageTenant(ctx, c.Job.MessageID)
		if err != nil {
			return noop, p.deferForPermit(c, "tenant_permit", "unavailable", permitUnknownDeferral, fmt.Errorf("worker: tenant lookup for job %s: %w", c.Job.ID, err))
		}
		tkey := p.permits.TenantPermitKey(tenant)
		got, err := p.permits.Acquire(ctx, tkey, p.permitPolicy.TenantLimit, p.permitPolicy.TTL, holder)
		if err != nil {
			return noop, p.deferForPermit(c, "tenant_permit", "unavailable", permitUnknownDeferral, fmt.Errorf("worker: tenant permit for job %s: %w", c.Job.ID, err))
		}
		if !got {
			return noop, p.deferForPermit(c, "tenant_permit", "deferred", permitDeferral, nil)
		}
		held = append(held, tkey)
		p.metrics.AbuseDecision("tenant_permit", "allowed")
	}

	dkey := p.permits.DestinationPermitKey(domain)
	got, err := p.permits.Acquire(ctx, dkey, p.permitPolicy.DestinationLimit, p.permitPolicy.TTL, holder)
	if err != nil {
		release()
		return noop, p.deferForPermit(c, "destination_permit", "unavailable", permitUnknownDeferral, fmt.Errorf("worker: destination permit for job %s: %w", c.Job.ID, err))
	}
	if !got {
		release()
		return noop, p.deferForPermit(c, "destination_permit", "deferred", permitDeferral, nil)
	}
	held = append(held, dkey)
	p.metrics.AbuseDecision("destination_permit", "allowed")
	return release, true
}

// deferForPermit releases the job with a jittered delay and reports ok=false.
func (p *Pool) deferForPermit(c queue.Claim, control, outcome string, base time.Duration, cause error) bool {
	if base == permitDeferral && p.permitPolicy.Deferral > 0 {
		base = p.permitPolicy.Deferral
	}
	p.metrics.AbuseDecision(control, outcome)
	if cause != nil {
		p.onError(cause)
		p.log.Warn("permit_state_unknown", "job_id", c.Job.ID, "message_id", c.Job.MessageID, "control", control)
	} else {
		p.log.Info("permit_deferred", "job_id", c.Job.ID, "message_id", c.Job.MessageID, "control", control)
	}
	now := p.now()
	delay := retry.BackoffPolicy{Base: base, Max: time.Hour, JitterPercent: p.permitPolicy.JitterPercent}.Jitter(base, now.UnixNano()+int64(len(c.Job.ID)))
	p.release(c, now.Add(delay))
	return false
}
