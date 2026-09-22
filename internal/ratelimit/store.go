// Package ratelimit is MailX's distributed abuse-control primitive layer: an
// atomic multi-bucket GCRA rate limiter and a lease-based concurrency permit set,
// both coordinated in Redis so every MailX process shares one truth.
//
// Division of labour: Redis holds only EPHEMERAL coordination state (a rate
// bucket is one number that expires on its own; a permit is a lease that expires on
// its own). PostgreSQL remains the durable truth for messages and for durable
// caps (queue depth). Nothing here decides fail-open versus fail-closed: every
// call returns an error on a Redis problem and the caller applies an explicit
// policy (see the API and worker), so a library error never silently disables a
// safety control.
//
// Algorithm choice: GCRA (the generic cell rate algorithm, equivalent to a token
// bucket). It stores ONE number per bucket (the theoretical arrival time), needs
// ONE Redis round trip and one script for any number of buckets, allows an exact
// burst, and yields an exact Retry-After. A sliding-window log would store one
// entry per request; a fixed window allows 2x bursts at boundaries and cannot give
// a truthful Retry-After.
//
// Keys (all under Namespace, default "mailx:limit"):
//
//	{ns}:rl:{<tenant>}:req          tenant request bucket        (1 key per ACTIVE tenant)
//	{ns}:rl:{<tenant>}:key:<key>    API-key guard bucket         (1 key per ACTIVE api key)
//	{ns}:rl:{<tenant>}:rcpt         tenant recipient bucket      (1 key per ACTIVE tenant)
//	{ns}:pt:tenant:<tenant>         tenant concurrency permits   (exists only while permits are held)
//	{ns}:pt:dest:<domain>           destination concurrency      (exists only while permits are held)
//
// <tenant> and <key> are MailX's opaque identifiers, never credentials. The
// {tenant} hash tag keeps one tenant's buckets in one Redis Cluster slot so the
// multi-key script is valid there. Every key carries a TTL, so an idle tenant
// leaves nothing behind; destination keys are bounded by the number of permits
// simultaneously held (at most the number of workers), not by attacker input.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultNamespace prefixes every key this package writes.
const DefaultNamespace = "mailx:limit"

// DefaultCallTimeout bounds each Redis round trip so a stalled Redis fails the
// decision quickly (the caller then applies its explicit failure policy) instead of
// hanging a request or a worker.
const DefaultCallTimeout = 250 * time.Millisecond

// Store talks to Redis. It is safe for concurrent use and holds no per-tenant state.
type Store struct {
	rdb         redis.Scripter
	ns          string
	callTimeout time.Duration
	// nowMicros, when set, replaces the Redis server clock. It exists for
	// deterministic tests only; production always uses the server clock, which is
	// the single time source shared by every MailX process.
	nowMicros func() int64
}

// NewStore returns a Store. namespace "" selects DefaultNamespace.
func NewStore(rdb redis.Scripter, namespace string) *Store {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	return &Store{rdb: rdb, ns: namespace, callTimeout: DefaultCallTimeout}
}

// WithClock returns a copy that uses now (microseconds since the epoch) instead of
// the Redis clock. Tests only.
func (s *Store) WithClock(now func() int64) *Store {
	c := *s
	c.nowMicros = now
	return &c
}

// WithCallTimeout returns a copy with a different per-call bound.
func (s *Store) WithCallTimeout(d time.Duration) *Store {
	c := *s
	if d > 0 {
		c.callTimeout = d
	}
	return &c
}

func (s *Store) clockArg() string {
	if s.nowMicros == nil {
		return ""
	}
	return strconv.FormatInt(s.nowMicros(), 10)
}

// ---- key builders -------------------------------------------------------------------

// TenantRequestKey is the tenant-wide request bucket.
func (s *Store) TenantRequestKey(tenantID string) string {
	return s.ns + ":rl:{" + tenantID + "}:req"
}

// APIKeyGuardKey is the per-API-key guard bucket (same slot as its tenant).
func (s *Store) APIKeyGuardKey(tenantID, apiKeyRowID string) string {
	return s.ns + ":rl:{" + tenantID + "}:key:" + apiKeyRowID
}

// TenantRecipientKey is the tenant-wide recipient-volume bucket.
func (s *Store) TenantRecipientKey(tenantID string) string {
	return s.ns + ":rl:{" + tenantID + "}:rcpt"
}

// TenantPermitKey and DestinationPermitKey name the concurrency permit sets.
func (s *Store) TenantPermitKey(tenantID string) string { return s.ns + ":pt:tenant:" + tenantID }
func (s *Store) DestinationPermitKey(domain string) string {
	return s.ns + ":pt:dest:" + domain
}

// ---- GCRA ---------------------------------------------------------------------------

// Bucket is one GCRA check. Rate is tokens per second, Burst the bucket size, Cost
// the tokens this operation consumes.
type Bucket struct {
	Key   string
	Rate  float64
	Burst int
	Cost  int
}

// Decision is the outcome of Allow.
type Decision struct {
	Allowed bool
	// RetryAfter is how long until the same request would be allowed (only when
	// denied and not Impossible). It is exact: it comes from the bucket state.
	RetryAfter time.Duration
	// Impossible means the cost exceeds the bucket's burst, so retrying can never
	// succeed (a configuration or caller bug, never a transient condition).
	Impossible bool
	// Denied is the index of the bucket that refused (first refusing bucket).
	Denied int
}

// gcraScript checks every bucket first and commits NONE unless ALL allow, so a
// request refused by the tenant bucket does not consume the API-key guard (and vice
// versa). Arguments: ARGV[1] clock override in microseconds (” = server clock),
// then per bucket: emission interval (µs), burst window (µs) and cost.
//
// Returns {allowed, retry_after_us, denied_index (1-based), impossible}.
var gcraScript = redis.NewScript(`
local now
if ARGV[1] ~= '' then
  now = tonumber(ARGV[1])
else
  local t = redis.call('TIME')
  now = tonumber(t[1]) * 1000000 + tonumber(t[2])
end
local n = #KEYS
local newtat = {}
local worst = 0
local denied = 0
local impossible = 0
for i = 1, n do
  local interval = tonumber(ARGV[2 + (i - 1) * 3])
  local window = tonumber(ARGV[3 + (i - 1) * 3])
  local cost = tonumber(ARGV[4 + (i - 1) * 3])
  local tat = tonumber(redis.call('GET', KEYS[i]))
  if (not tat) or tat < now then tat = now end
  local nt = tat + interval * cost
  newtat[i] = nt
  if interval * cost > window then
    if denied == 0 then denied = i end
    impossible = 1
  else
    local allow_at = nt - window
    if now < allow_at then
      if denied == 0 then denied = i end
      if allow_at - now > worst then worst = allow_at - now end
    end
  end
end
if denied ~= 0 then
  return {0, math.ceil(worst), denied, impossible}
end
for i = 1, n do
  local ttl_ms = math.ceil((newtat[i] - now) / 1000) + 1000
  redis.call('SET', KEYS[i], string.format('%.0f', newtat[i]), 'PX', ttl_ms)
end
return {1, 0, 0, 0}
`)

// Allow atomically evaluates all buckets. All must allow for any to be charged. On
// a Redis error it returns (Decision{}, err) and charges nothing.
func (s *Store) Allow(ctx context.Context, buckets ...Bucket) (Decision, error) {
	if len(buckets) == 0 {
		return Decision{Allowed: true}, nil
	}
	keys := make([]string, len(buckets))
	args := make([]any, 0, 1+3*len(buckets))
	args = append(args, s.clockArg())
	for i, b := range buckets {
		if b.Rate <= 0 || b.Burst < 1 || b.Cost < 1 {
			return Decision{}, errors.New("ratelimit: invalid bucket")
		}
		interval := 1e6 / b.Rate
		keys[i] = b.Key
		args = append(args, strconv.FormatFloat(interval, 'f', 3, 64), strconv.FormatFloat(interval*float64(b.Burst), 'f', 3, 64), b.Cost)
	}
	cctx, cancel := context.WithTimeout(ctx, s.callTimeout)
	defer cancel()
	res, err := gcraScript.Run(cctx, s.rdb, keys, args...).Int64Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("ratelimit: rate check: %w", err)
	}
	if len(res) != 4 {
		return Decision{}, errors.New("ratelimit: unexpected script reply")
	}
	if res[0] == 1 {
		return Decision{Allowed: true}, nil
	}
	return Decision{RetryAfter: time.Duration(res[1]) * time.Microsecond, Denied: int(res[2]) - 1, Impossible: res[3] == 1}, nil
}

// ---- permits ------------------------------------------------------------------------

// acquireScript prunes expired leases, then grants a permit only while fewer than
// `limit` leases are held. The lease expiry is the crash-recovery mechanism: a
// worker that dies never releases, and its permit disappears on its own.
var acquireScript = redis.NewScript(`
local now
if ARGV[1] ~= '' then now = tonumber(ARGV[1]) / 1000 else
  local t = redis.call('TIME'); now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
end
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
local limit = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
if redis.call('ZSCORE', KEYS[1], ARGV[4]) then
  redis.call('ZADD', KEYS[1], now + ttl, ARGV[4])
  redis.call('PEXPIRE', KEYS[1], ttl)
  return 1
end
if redis.call('ZCARD', KEYS[1]) >= limit then return 0 end
redis.call('ZADD', KEYS[1], now + ttl, ARGV[4])
redis.call('PEXPIRE', KEYS[1], ttl)
return 1
`)

// Acquire takes one permit under key if fewer than limit are held. holder must be
// unique per attempt (a job id + claim token). Acquiring again with the same holder
// refreshes its lease. ttl bounds how long a crashed holder blocks others.
func (s *Store) Acquire(ctx context.Context, key string, limit int, ttl time.Duration, holder string) (bool, error) {
	if limit < 1 || ttl <= 0 || holder == "" {
		return false, errors.New("ratelimit: invalid permit request")
	}
	cctx, cancel := context.WithTimeout(ctx, s.callTimeout)
	defer cancel()
	n, err := acquireScript.Run(cctx, s.rdb, []string{key}, s.clockArg(), limit, ttl.Milliseconds(), holder).Int64()
	if err != nil {
		return false, fmt.Errorf("ratelimit: acquire permit: %w", err)
	}
	return n == 1, nil
}

// Release returns a permit. It is idempotent and bounded; a failure is safe because
// the lease expires by itself.
func (s *Store) Release(ctx context.Context, key, holder string) error {
	rc, ok := s.rdb.(redis.Cmdable)
	if !ok {
		return errors.New("ratelimit: store cannot release")
	}
	cctx, cancel := context.WithTimeout(ctx, s.callTimeout)
	defer cancel()
	if err := rc.ZRem(cctx, key, holder).Err(); err != nil {
		return fmt.Errorf("ratelimit: release permit: %w", err)
	}
	return nil
}

// Held reports the number of live leases under key (for tests and diagnostics).
func (s *Store) Held(ctx context.Context, key string) (int64, error) {
	rc, ok := s.rdb.(redis.Cmdable)
	if !ok {
		return 0, errors.New("ratelimit: store cannot count")
	}
	return rc.ZCard(ctx, key).Result()
}
