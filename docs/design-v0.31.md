# MailX v0.31 — Outbound Abuse Controls & Sending Safety

Scope (CR-014): minimal, meaningful controls that make legitimate sending safer. Not in scope: spam-filter evasion,
content scoring/ML, billing/plans, sender reputation/warm-up, complaint/feedback loops (v0.32), inbound.

## Threat model (outbound)

| # | Threat | Control |
|---|--------|---------|
| T1 | A compromised or buggy client floods the API | per-tenant request bucket (shared by every key of the tenant) |
| T2 | One key spends the whole tenant allowance | per-key guard bucket (<= tenant limit) |
| T3 | Minting more API keys to multiply the limit | tenant bucket is charged by every key; keys never add capacity |
| T4 | Mass mailing through one account (recipient volume) | per-tenant recipient bucket, charged per DELIVERABLE recipient |
| T5 | One tenant fills the queue / starves others | per-tenant undelivered cap (429) + global backlog cap (503) + round-robin dispatch |
| T6 | One tenant or one slow destination occupies every worker | per-tenant and per-destination concurrency permits |
| T7 | Retry storm after a remote outage | deterministic bounded jitter on retry times and permit deferrals |
| T8 | Retry-on-refusal doubles charges / burns idempotency keys | replays never charged; refused sends release their claim |
| T9 | Redis outage becomes unmetered sending | sending fails CLOSED (503 + Retry-After); reads fail open |
| T10 | Limiter state grows without bound | every key has a PX TTL; cardinality bounded per tenant/key/domain |
| T11 | Limits leak tenant data or become a metrics-cardinality bomb | closed label sets; refusal bodies carry no tenant/address data |

Non-goals stated plainly: this is not a reputation system, does not detect spam content, and cannot stop a legitimate
tenant with valid recipients from sending unwelcome mail within its limits.

## Where each control sits (POST /v1/emails)

1. authenticate (unauthenticated requests never touch a bucket; no Retry-After leaks)
2. request limiter (tenant + key buckets, ONE atomic script, all-or-nothing)
3. validation, authorization, DKIM signing, suppression check (returns the DELIVERABLE count)
4. idempotency claim (replay -> return original, never charged; different payload -> 409, never charged)
5. `admitSend`, in order of least side effect: tenant queue cap (429 `tenant_queue_full`), global backlog (503 `system_busy`),
   recipient bucket charge (429 `recipient_rate_limited`). A refusal releases the idempotency claim it owns.
6. FileStore write, transactional insert (message + recipients + outbox + event), 202.

Worker: after suppression and BEFORE the coordinator/transport, take the tenant permit then the destination permit;
refusal => release the job with a jittered delay, record NO attempt, move NO retry counter, do NO SMTP.

## Algorithm: GCRA in one atomic Redis Lua script (`internal/ratelimit`)

GCRA is the token bucket expressed as a single "theoretical arrival time" (TAT) per key: O(1) state, exact
`Retry-After`, no refill timer. Multi-bucket calls check every bucket first and commit none unless all allow (a refused
request costs nothing). Time is Redis `TIME` (one clock for every MailX process); tests inject a clock. Each call has a
context timeout. Keys use `{tenant}` hash tags so multi-key calls stay on one Redis Cluster slot.
Redis operations per limiter decision: exactly one `EVALSHA` (one round trip) regardless of bucket count.

Concurrency permits are a ZSET of holders scored by expiry: prune-on-acquire, idempotent re-acquire per holder, PEXPIRE on
the key. A crashed worker's permit expires by TTL (`MAILX_LIMIT_PERMIT_TTL`, default 10 m, must exceed the longest attempt).

## Failure policy (explicit)

| Situation | Behaviour | Why |
|-----------|-----------|-----|
| Limiter (Redis) error on a send / mutating request | 503 `rate_limiter_unavailable`, Retry-After 5 | an outage must not become unlimited sending; matches the auth/readiness precedent |
| Limiter error on GET | proceed (fail open, metric+log) | reads cannot send mail; refusing them makes a Redis blip a full outage |
| Capacity count query error (PostgreSQL) | 500 | never guess capacity |
| Permit state unknown in the worker | defer 15 s, no attempt | fail-closed for delivery, but the message is never failed for an infrastructure blip |
| `MAILX_ABUSE_CONTROLS=off` | no controls, loud startup warning | explicit local-development opt-out; any other non-on value fails startup |

## 429 vs 503

429 = the tenant/key exceeded ITS limit (`tenant_rate_limited`, `api_key_rate_limited`, `recipient_rate_limited`,
`tenant_queue_full`); Retry-After is exact for rate limits and a fixed 30 s for queue-full. 503 = MailX is at capacity
(`system_busy`) or cannot enforce limits (`rate_limiter_unavailable`); it is nobody's fault and applies to every tenant.

## Idempotency interaction

Replays (same key + same body) and 409s (same key + different body) are decided before any charge. A request that owns a
claim and is then refused by an abuse control DELETEs that claim (guarded by fingerprint + `in_progress`), so the client
retries the SAME key after Retry-After. Charges happen at acceptance; a rare failure after the charge (PostgreSQL write
failing) wastes tokens that refill on their own.

## Fair dispatch (migration 000014)

Old: `ORDER BY available_at LIMIT n` over all tenants (one big backlog fills every batch). New: a recursive loose index scan
finds each tenant with due work (one index probe per tenant, capped at 200), a LATERAL subquery takes each tenant's oldest
rows, and rows are ranked per tenant and interleaved (rank 1 of every tenant, then rank 2, ...). `FOR UPDATE SKIP LOCKED` is
removed: each read was its own implicit transaction, so the lock released immediately; duplicate-safe Enqueue and the guarded
`MarkOutboxDispatched` remain the correctness mechanism. The migration adds `idx_outbox_pending_tenant (tenant_id, available_at)
WHERE dispatched_at IS NULL` and DROPS `idx_outbox_pending`: while both existed the planner preferred the global index and
filtered by tenant, reading 15,000 rows behind a 30,000-row backlog. One index also halves outbox write cost/memory.

EXPLAIN (ANALYZE, BUFFERS) with a 30,000-row backlog for tenant A and 3 newer rows for tenant B: tenant discovery is Index Only
Scans on `idx_outbox_pending_tenant`; the per-tenant probe is an Index Scan on the same index; execution 0.062 ms (vs 1.5 ms with
both indexes); B's 3 rows are in the first batch of 100. Test: `TestFairDispatchPlanAtScale`.

Known limit: more than 200 simultaneously backlogged tenants are served in tenant-id order per batch (each dispatched row leaves
the set, so progress continues; fairness among >200 backlogged tenants is approximate) — RSK-031.

## Queue semantics

The Redis queue orders by AvailableAt and `Enqueue` blocks at capacity (`MAILX_QUEUE_CAPACITY`), which bounds queue churn.
`MAILX_LIMIT_MAX_PENDING_DISPATCH` is the API-side backpressure so the outbox does not grow without bound while the queue is
full. Per-tenant caps use bounded counts (`SELECT count(*) FROM (... LIMIT cap)`), never a full count.

## Redis keys and memory

`mailx:limit:rq:{tenant}` request, `:kg:{tenant}:{key}` key guard, `:rr:{tenant}` recipients, `:pt:tenant:{t}` and `:pd:dest:{domain}`
permits. Each GCRA key is one string with a PX TTL equal to its refill time; a permit set has PEXPIRE. Steady-state cost is
bounded by (active tenants x 3) + (active API keys) + (concurrently used destination domains); an idle tenant's keys expire by
themselves. Tested: TTL cleanup, abandoned-permit cleanup. No Redis memory settings changed; the low-memory PostgreSQL profile is
unaffected (the limiter uses Redis, not PostgreSQL; PostgreSQL gains one bounded-count query per send and one fair dispatch query
per tick).

## Retry jitter

`BackoffPolicy.JitterPercent` (0-50, default 10 in production): retry delay is spread by +/- N% using an FNV hash of the scheduling
instant (deterministic, bounded, never above Max, never below 1 s). 500 failures inside one second spread over >100 distinct
seconds with <=20 in any second (`TestJitterBreaksRetryStorms`). Permit deferrals use the same helper.

## Metrics and logs

`mailx_abuse_control_decisions_total{control,outcome}`: control in request|recipient|tenant_queue|backpressure|tenant_permit|
destination_permit, outcome in allowed|limited|unavailable|impossible|deferred, anything else -> `other` (max 30 series). Logs carry
route class / job id / message id / control name only: never tenant ids, key ids, addresses, or domains.

## Measured

`BenchmarkAllowTwoBuckets`: 312 us/op (1 CPU), 91 us/op (4), 44 us/op (12) against Redis in a Docker VM (round-trip dominated).
Request path: 1.9 us without the limiter vs 314 us with it at 1 CPU (94 us at 4): +44-30 allocations, +1.4 KB. Redis-side EVALSHA ~34 us.
`ListPendingOutbox` fair, 20 tenants x 100 rows, limit 100: 1.2 ms/op end to end. `CountTenantQueued` with 500 rows: 0.30 ms/op.

## Configuration

See `.env.example`. Invalid values fail startup naming the variable; defaults are `ratelimit.DefaultPolicy()`:
50/100 tenant rps/burst, 25/50 key, 20/500 recipients, 50 per message, 10,000 queued per tenant, 100,000 backlog, 16/16 concurrency,
10 m permit TTL, 10% jitter. Cross-field rules (`Policy.Validate`): key limit <= tenant limit; recipient burst >= max recipients.

## Limitations

- Limits are per account, not per sending domain or per recipient domain (destination concurrency is per domain; destination RATE is not limited).
- Request limits apply to every authenticated /v1 route uniformly (no per-route weights).
- No warm-up ramp, no automatic tenant suspension, no complaint/bounce-rate circuit breaker.
- Charged recipient tokens are not refunded if PostgreSQL fails after the charge.
- A tenant at the queue cap can still read, list and manage suppressions.
