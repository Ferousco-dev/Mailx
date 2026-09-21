# MailX Architecture, Invariants, Security, Limitations (current through v0.26)

Verified against HEAD `481da4a` on 2026-09-21. This file states CURRENT truth. Superseded designs are kept only
under "Superseded" so they are never read as current. History lives in `milestones.md`; rationale IDs in `decisions.md`.
Update this file whenever a task changes anything listed here (see `CLAUDE.md`).

## Purpose

MailX: open-source, developer-first email infrastructure written from the protocol up in Go (module `github.com/Ferousco-dev/mailx`,
Go 1.27.1; deps: pgx/v5, go-redis/v9, x/net). It began as a Go-learning project and grew into an owned SMTP/delivery/API stack.

## Runtime shape

One binary `cmd/mailx`.
- No args + no `DATABASE_URL`: `serve()` = SMTP receiver + FileStore only (original v0.1-v0.14 behavior).
- No args + `DATABASE_URL`: `runFull()` runs components under one shutdown signal: `smtp`, `dispatch`, `worker`,
  `webhook-fanout`, `webhook-worker`, `api`, `idempotency-cleanup`. **Any component failing or exiting stops the whole process** (`runComponents`).
- Subcommands: `list`, `inspect <id>`, `migrate`, `create-tenant`, `create-api-key`, `rotate-api-key`, `revoke-api-key`, `list-api-keys`.
- Migrations run at startup (`openDatabase` -> `db.Migrate`) and via `mailx migrate` (Compose one-shot service).

## Main flow (API send)

```
Developer -> REST API (auth, scope, validate, optional Idempotency-Key)
  -> FileStore write of raw MIME (BEFORE the DB tx; orphan file possible if tx fails)
  -> PostgreSQL tx: message + recipients + 'queued' event + outbox row (+ idempotency completion)   => HTTP 202
  -> dispatch poller: due outbox rows -> Redis queue (duplicate-safe Enqueue; MarkOutboxDispatched)
  -> worker pool (N goroutines): claim -> load DURABLE state -> retry.Coordinator.Attempt
       -> delivery.Engine (DNS/MX -> transfer -> SMTP client)
  -> OutcomeStore.Persist (ONE PG tx): delivery_attempts row + messages.status + immutable event
  -> only then queue Ack / Release(next_retry_at)
  -> webhook fan-out (events.fanned_out_at IS NULL -> webhook_deliveries, one PG tx)
  -> webhook worker pool: lease claim -> decrypt secret -> signed HTTPS POST -> complete attempt
```
SMTP receive path (`:2525`) only parses and stores to FileStore; it does NOT create DB rows, outbox entries, or queue jobs (verified: `runSMTPReceiver` -> FileStore).

## Component map

| Package | Role |
| --- | --- |
| `internal/smtp` | Protocol server (state machine, limits) and outbound client (`client*.go`, dot-stuffing) |
| `internal/mail` | `Envelope` vs `Message`, MIME parser |
| `internal/storage` | `FileStore` (raw `.eml`, metadata, attachments) |
| `internal/transfer` | One SMTP attempt -> `Result` |
| `internal/dns` | MX resolution |
| `internal/delivery` | Synchronous MX-walking delivery |
| `internal/retry` | Decide, backoff, attempt limit, `State`, `Coordinator` |
| `internal/bounce` | Classification + DSN generation (no sending) |
| `internal/queue` | `Queue` interface; `MemoryQueue`, `RedisQueue` (Lua) |
| `internal/worker` | Bounded pool + `OutcomeStore` boundary |
| `internal/dispatch` | Outbox -> queue poller |
| `internal/database` | pgx repositories + 11 migrations |
| `internal/api` | stdlib net/http v1 API, OpenAPI, middleware |
| `internal/auth` | API keys |
| `internal/idempotency` | Key validation + request fingerprint |
| `internal/outbound` | Build outbound MIME from API request |
| `internal/domain` | Domain ownership (DNS TXT) |
| `internal/webhook` | Subscriptions, secrets, signing, URL policy, fan-out, delivery workers |
| `internal/observability` | slog logger factory (panic-safe), Prometheus metrics (private pedantic registry, bounded labels), operator listener (`/metrics`, `/health/live`, `/health/ready`), bounded readiness |
| `internal/buildinfo` | Version/commit via `-ldflags -X`; fallbacks `dev`/`unknown` |
| `cmd/mailx` | Composition root (`databaseOutcomeStore` adapter lives here so `database` does not import `delivery`/`retry`) |

## HTTP surface (v1)

Auth: `Authorization: Bearer mx_<key_id>_<secret>` (not a JWT). Scopes: `emails:send`, `emails:read`, `domains:read`, `domains:write`,
`webhooks:read`, `webhooks:write`. Unauthenticated: `/health/live`, `/health/ready` (readiness = PostgreSQL AND Redis within 2 s total; 503 body names only `postgres`/`redis`, never raw errors), `/openapi.json`, `/docs`.
Operator listener (`MAILX_OBSERVABILITY_ADDR`, default `:9090`, separate from the API): `GET /metrics`, `/health/live` (no dependency calls), `/health/ready`; no `/v1`.
Routes: `POST /v1/emails`, `GET /v1/emails[/{id}]`, `/v1/domains` (+`/{id}`, `/{id}/verify`, DELETE), `/v1/webhooks` (+`/{id}`, `/{id}/rotate-secret`, `/{id}/deliveries`, DELETE), `GET /v1/events`.
`POST /v1/emails` in v0.22: text/html body, to/cc/bcc, reply-to; **all recipients must share one domain (422 `mixed_recipient_domains`)**; attachments/tags/custom headers deferred.
OpenAPI is hand-written (`internal/api/openapi.go`) and guarded by a runtime drift test: API changes must update it.

## Database (PostgreSQL 16; migrations 000001-000011; forward-only fixes, applied migrations never edited)

Tables: `tenants`, `messages`, `recipients`, `delivery_attempts`, `events`, `outbox`, `api_keys`, `idempotency_keys`, `domains`,
`webhook_subscriptions`, `webhook_deliveries`, `webhook_delivery_attempts`. Message statuses: `queued, processing, retrying, delivered, failed, bounced`.
Event types (internal): `queued, delivery_attempted, delivered, deferred, bounced, failed`; public mapping: `email.queued, email.delivered, email.delivery_delayed, email.failed, email.bounced` (`delivery_attempted` is never public).
IDs: application-generated crypto-random hex TEXT (no sequential IDs; avoids enumeration/volume leaks). `tenant_id` on every tenant-owned table.
Raw MIME and attachments are not in PostgreSQL. Indexes are justified by query patterns and checked by EXPLAIN tests (`explain_test.go`).
Note: migration 000011 `webhook_deliveries` FKs `events(tenant_id,id) ON DELETE CASCADE`; subscriptions use `ON DELETE RESTRICT` (history retained; disable, not delete).

## Configuration (environment)

`DATABASE_URL` (enables full pipeline), `REDIS_ADDR` (default localhost:6379), `MAILX_HTTP_ADDR` (:8080), `MAILX_SMTP_ADDR` (:2525),
`MAILX_STORAGE_ROOT`, `MAILX_WORKERS` (4), `MAILX_QUEUE_CAPACITY` (1000), `MAILX_CLAIM_LEASE` (2m), `MAILX_API_KEY_PEPPER` (optional; warns if unset),
`MAILX_WEBHOOK_MASTER_KEY` (**required** in full mode; base64 32 bytes), `MAILX_WEBHOOK_ALLOW_INSECURE` (dev only: allows HTTP + private IPs), `MAILX_WEBHOOK_WORKERS` (4), `MAILX_LOG_LEVEL` (info), `MAILX_LOG_FORMAT` (json|text), `MAILX_OBSERVABILITY_ADDR` (:9090; explicitly empty disables).
Tests: `MAILX_TEST_DATABASE_URL` (PostgreSQL DSN; if unset, a local default is tried and tests skip when unreachable) and `REDIS_ADDR` (Redis tests fail loudly rather than skip). CI runs `go test -race ./...` with Postgres/Redis services + govulncheck.
`.env` and `.claude` are gitignored; `.env.example` holds dev-only placeholders.

## Correctness invariants (each verified in code/tests/history)

1. **Accepted=true is never knowingly retransmitted.** `Accepted` = 2xx to DATA terminator (`internal/smtp/client.go`); `retry.Decide` returns TerminalSuccess whenever `Accepted` even if an error is present; delivery never opens a second connection after acceptance; worker reads durable state before SMTP and skips terminal messages.
2. **QUIT failure after DATA acceptance does not fail delivery** (`QuitError` recorded; engine returns nil error).
3. **MX fallback only for pre-session failures** (dial/greeting/EHLO/HELO); after MAIL/RCPT/DATA engagement the remote answer is final.
4. **Queue finalization (Ack/Release) happens only after the delivery outcome is durable.** `Pool.persistOutcome` retries PostgreSQL only (never re-enters SMTP), renews the claim lease each attempt, and on shutdown leaves the claim unfinalized for reclaim.
5. **PostgreSQL owns durable lifecycle truth.** Redis holds only scheduling + claim ownership; `retry.State` is rebuilt from `delivery_attempts` on reclaim (`databaseOutcomeStore.Load`, which also cross-checks stored decision vs result kind).
6. **Attempt + message status + event are one transaction** (`PersistDeliveryOutcome`), under `SELECT .. FOR UPDATE` on the message; replay of the same attempt is idempotent success; a new attempt after terminal returns `ErrMessageTerminal` (worker Acks, no SMTP). One outcome event per attempt (unique index, migration 000010).
7. **Transactional outbox:** HTTP 202 means message + outbox row are committed; dispatch to Redis is retried until done, independent of Redis availability at accept time.
8. **Retry is conservative:** unknown/zero-value decision = terminal; caller cancel/deadline = terminal; only temporary DNS/transfer failures retry; default 5 total operations, 30m base, 4h cap (values are policy defaults, not RFC mandates).
9. **Queue semantics are at-least-once**, duplicate active job ID is a no-op, Ack/Release/Renew are fenced by an ownership token (stale token rejected atomically in Lua).
10. **Tenant isolation:** every API resource is tenant-scoped in the query (tenant identity comes only from `authenticateMiddleware` via request context, never from client input). Idempotency scope is the tenant, not the key row, so rotation never breaks retries.
11. **Idempotency:** claim after validation, before FileStore write; completion in the same tx as message/outbox; completion requires matching fingerprint; different payload with same key => 409 (never echoes original); PostgreSQL is the sole arbiter.
12. **Event truth vs webhooks:** `events` is the only lifecycle source of truth; webhook tables track only notification obligations/attempts. A webhook failure cannot change message/attempt/event rows (separate tables; fan-out only sets `fanned_out_at`).
13. **Webhook delivery is at-least-once.** Stable event ID (`MailX-Event-Id`, envelope `id`) survives retries; consumers must deduplicate by `event_id`. `UNIQUE(subscription_id, event_id)` prevents double fan-out.
14. **Fan-out is atomic:** delivery rows + `fanned_out_at` in one tx (`FOR UPDATE SKIP LOCKED`); a subscription only receives events with `occurred_at >= subscription.created_at` (no backfill).
15. **Customer endpoints never execute in the email delivery path:** delivery worker and webhook workers are separate pools; the only coupling is the `events` table.
16. **Webhook lease fencing:** `lease_token` guards `CompleteWebhookAttempt`; expired leases are reclaimed and the abandoned attempt is marked `retrying`/`lease_expired`; CHECK constraint ties `delivering` to lease columns.
17. **DSN safety:** DSNs use null reverse-path; a DSN is never generated for a null-sender message; DSN carries original headers only, never the body.
18. **Recipient tracking is envelope-based**, not header-based (hidden Bcc is envelope-only; `header_kind` NULL).

## Observability (v0.23; observers only, never a source of truth)

- Logs: `log/slog` JSON (default) or text on stderr. Fields are stable IDs and bounded categories only: request_id, tenant_id, message_id, job_id, attempt, event_id, delivery_id, subscription_id, smtp `session_id` (process-local, no security meaning). Never logged: bodies/MIME, addresses, domains, URLs, secrets, signatures, Authorization, remote SMTP/HTTP text, panic values. A panicking log sink is swallowed (`safeHandler`).
- HTTP: request-ID middleware is OUTERMOST (recover is inside it) so panics still produce one access log + metric; route label is the matched mux pattern or `unmatched`.
- Components take optional `WithLogger`/`WithMetrics` (default discard/nil); `smtp.Config.Observer` for SMTP; `RedisQueue.Ping/Depth` are concrete read-only methods (`queue.Queue` unchanged).
- Metrics (namespace `mailx`, <=3 labels, allowlisted values else `other`): `build_info{version,commit}`, `http_requests_total{method,route,status_class}`, `http_request_duration_seconds{method,route}`, `smtp_sessions_total{result}`, `smtp_active_sessions`, `smtp_messages_total{result}`, `delivery_attempts_total{kind,decision}`, `delivery_attempt_duration_seconds{decision}`, `queue_operations_total{operation,result}`, `queue_depth`, `queue_depth_errors_total`, `webhook_attempts_total{outcome}`, `webhook_attempt_duration_seconds{outcome}`, plus Go/process collectors.
- The operator listener also runs in SMTP-only mode (no `DATABASE_URL`); readiness there has no dependencies to check.
- Webhook claim: one active claim per tenant is enforced with a per-tenant advisory lock plus a re-check after the lock (READ COMMITTED), not just a NOT EXISTS guard. Webhook creation maps resolver failures to a stable 503 and never returns resolver detail.
- Invariants: metrics/logging failure never alters SMTP, queue, DB, event, or webhook behavior; no logs table, no migrations (still 11).
- Worker errors passed to `WithOnError` are internal-infrastructure errors only; the recipient address was removed from the worker's "not addressable" error text.

## Outbound SMTP TLS (v0.24; design: `docs/design-v0.24.md`)

- Flow: 220 -> EHLO (HELO fallback on 5xx) -> capabilities -> policy -> `STARTTLS` (must be 220) -> reject if bytes follow the 220 -> `s.caps = nil` -> `tls.Client(raw).HandshakeContext` (bounded) -> swap I/O to TLS conn -> EHLO again (no HELO fallback) -> new capabilities -> MAIL/RCPT/DATA. `raw` never changes (deadline watcher race-free); `conn` is the I/O conn.
- Policy `MAILX_SMTP_TLS_POLICY`: `opportunistic` (default: TLS if advertised, plaintext only when NOT advertised) or `required` (never sends a message without verified TLS; fails before MAIL FROM). No silent downgrade: refusal (even 5xx), malformed reply, handshake or verification failure, or post-TLS EHLO failure fails the attempt; no plaintext retry on that connection or by reconnecting.
- Verification always on: system roots (+ `MAILX_SMTP_TLS_CA_FILE` extra roots), validity, server-auth EKU, MX host name (`ServerName` = host of the MX address). NOT verified: that the MX is the right MX for the recipient domain (unauthenticated DNS; no DANE/MTA-STS). No `InsecureSkipVerify` in production code. TLS min 1.2 (equals Go default).
- Bounds: `DialTimeout`, per-read `ReadTimeout`, `TLS.HandshakeTimeout` (30 s default), caller context (cancel sets a past deadline on the raw conn; close_notify write bounded to 1 s).
- Errors: stages `starttls`, `tls_handshake`, `ehlo_tls`; always `Temporary`; `TLSFailure.Error()` is a bounded category (raw x509/TLS text only via Unwrap). Delivery engine `decideFallback` returns `tryNext` for these stages (all before MAIL FROM); if all MX fail, kind is `KindTransferTemporary` and the existing retry engine reschedules.
- Observability: `mailx_smtp_tls_sessions_total{policy,outcome,version}` (allowlisted; 11 outcomes: established, not_offered, required_unavailable, rejected, handshake_timeout, verify_failed, handshake_failed, connection_lost, ehlo_failed, canceled, protocol_error; versions 1.2/1.3/other/none); worker `delivery_outcome` adds `tls_policy`, `tls_outcome`, `tls_version` from the last MX tried.
- Invariants added: capabilities from before a handshake are never used after it; `Accepted=true` over TLS is never retransmitted even if QUIT/teardown fails (tested at client and worker-pipeline level); liveness/readiness never depend on remote SMTP/TLS; the inbound listener does not advertise STARTTLS (tested).
- Deployment: the Docker runtime image installs `ca-certificates` (alpine ships none, which would have broken all peer verification including HTTPS webhooks).

## Outbound SMTP AUTH / trusted relay (v0.25; design: `docs/design-v0.25.md`)

- Flow: ... EHLO again (post-TLS) -> AUTH advertised in THAT reply? -> PLAIN (preferred) or LOGIN -> 235 -> MAIL FROM. Credentials imply TLS-required regardless of `MAILX_SMTP_TLS_POLICY`; no STARTTLS, refused STARTTLS, handshake or certificate failure all end the attempt before any AUTH byte. Mechanism chosen only from post-TLS capabilities (both `AUTH x y` and legacy `AUTH=x y` spellings). PLAIN uses the initial response unless the command would exceed the line limit, then the challenge form. CRAM-MD5, DIGEST-MD5, XOAUTH2/OAUTHBEARER, SCRAM unsupported.
- Routing: `delivery.Config.Relay` (nil = direct). Relay mode routes every delivery to the one configured relay (no MX lookup, no credentials-bearing path anywhere else, no fallback to direct on any failure). Direct mode resolves MX hosts and carries no credentials. `delivery.Result.Transport` = direct|relay. Config: `MAILX_RELAY_HOST` (enables), `MAILX_RELAY_PORT` (587), `MAILX_RELAY_USERNAME`, `MAILX_RELAY_PASSWORD`; invalid combinations are startup errors that name variables, never values. Compose and `.env.example` carry no credentials.
- Errors/retry: stage `auth`; SMTP semantics kept on the DeliveryError (454 temporary, 535/534 permanent, transport failures temporary); the engine treats every auth failure as temporary (`stopTemporary`, never another MX) because it describes relay configuration, not the message; existing backoff (30m..4h, 5 operations) bounds retries (tested: 20 jobs with wrong credentials = one AUTH attempt each, retries >= 20 minutes out). No separate AUTH retry system.
- Secrets: `smtp.Credentials` redacts under every fmt verb and slog; remote AUTH reply text is discarded (codes only); per-Send and per-engine copies; value-free validation errors (255-byte limit, no CR/LF/NUL). Base64 payloads are treated as secrets and searched for in logs, metrics, errors and persisted results in tests.
- Observability: `mailx_smtp_auth_attempts_total{mechanism (plain|login|none), outcome (success|rejected|temporary|no_tls|not_advertised|no_mechanism|protocol_error|connection_lost|timeout|canceled)}`; worker `delivery_outcome` adds `transport`, `auth_mechanism`, `auth_outcome`. Liveness/readiness never touch the relay.
- Invariants added: credentials never sent over plaintext or on stale pre-TLS capabilities; MAIL FROM unreachable before 235; direct MX delivery never authenticates (tested with a real MX that offers AUTH); relay failure never becomes direct delivery; AUTH success is not delivery (Accepted still needs final DATA 2xx; QUIT failure after acceptance keeps Accepted=true through the relay path).

## DKIM and verified-From (v0.26; design: `docs/design-v0.26.md`)

- Verified-From: `POST /v1/emails` requires the RFC 5322 From domain (parsed with `net/mail`, canonicalized by `domain.Normalize` via `domain.FromDomain`) to EXACTLY equal a verified, non-deleted domain of the authenticated tenant (`db.VerifiedSenderDomain`). No suffix or parent/subdomain inference; another tenant's or an unverified domain gives 403 `from_domain_not_authorized`. Runs before the idempotency claim, the FileStore write and every insert; `InsertMessage(SenderDomain)` re-checks FOR SHARE in its transaction. MAIL FROM is still the From address (bounce architecture unchanged). The API is the only outbound creator.
- Signing: `dkim.Sign` = rsa-sha256, 2048-bit, relaxed/relaxed, d=From domain (must equal the message From domain, exactly one From), signed headers from,to,cc,subject,date,message-id,mime-version,content-type,content-transfer-encoding,reply-to (present ones), tags v,a,c,d,s,t,h,bh,b only. Done at acceptance in the API after MIME is final and before storage: stored bytes = queued bytes = transmitted bytes (tested at a capturing MX for direct and relay, plus independent verification with go-msgauth). Retries resend the stored bytes; a message keeps its acceptance-time signature across key rotation.
- Keys: `dkim_keys` (migration 000012; FK to `domains(tenant_id,id)`; CHECK algorithm/size/selector/lifecycle; `uq_dkim_one_active`, `uq_dkim_one_pending`; `UNIQUE(domain_id,selector)`). Lifecycle pending (does not sign) -> active (after DNS TXT publication is verified by `POST .../dkim/verify`) -> retired (private ciphertext destroyed). Rotation = create a new pending key, publish, verify; the old key signs until activation. Selector `mx`+YYYYMMDD+4 hex. Domain deletion deletes keys in the same transaction; a re-claimed name starts with no keys.
- Storage/crypto: private key PKCS#8 DER -> AES-256-GCM (`internal/secretbox`, fresh nonce, AAD `mailx-dkim-v1|tenant|domain|selector`). `MAILX_DKIM_MASTER_KEY` (required, base64 32 bytes) must differ from `MAILX_WEBHOOK_MASTER_KEY` (enforced at startup); errors name variables not values. Webhook `SecretBox` now wraps the shared `secretbox`. Private keys are never in any API response, log, metric or error; keys live in process memory while used (Go cannot zeroize).
- Key creation is guarded: a domain with a pending key returns 409 before any RSA generation, and at most 2 generations run concurrently (cancellable wait).
- Failure behavior: a domain with an ACTIVE key always signs; missing/undecryptable/corrupt/mismatched key or signing failure => 503 `dkim_signing_unavailable`, nothing accepted or stored, never unsigned. A domain with no active key sends unsigned (DKIM not set up).
- API: `GET|POST /v1/domains/{id}/dkim`, `POST /v1/domains/{id}/dkim/verify` (domains:read/write, tenant-scoped 404); OpenAPI updated (drift test).
- Observability: `mailx_dkim_signatures_total{algorithm (rsa-sha256), outcome (signed|unsigned_no_key|key_unavailable|key_decrypt_failed|key_invalid|sign_failed|domain_mismatch)}`; nothing domain-, selector- or key-derived is logged or labelled.

## Security decisions (verified)

- API secrets never stored raw: only `key_id` + HMAC-SHA256(pepper, secret); 256-bit random secret makes slow password hashing pointless; unkeyed SHA-256 fallback without pepper is a documented dev-only choice. All auth failures collapse to one generic 401; DB error on auth fails closed. No public key-creation endpoint (CLI bootstrap).
- Webhook signing secrets need recoverable storage => AES-256-GCM with `MAILX_WEBHOOK_MASTER_KEY`, tenant ID as associated data (`webhook/secret.go`); secret returned only at create/rotate; `whsec_` prefix.
- Signature: `MailX-Webhook-Signature: v1=hex(HMAC-SHA256(secret, timestamp + "." + body))`, signed bytes = sent bytes; `VerifySignature` enforces 5-minute tolerance and constant-time compare.
- SSRF: `URLPolicy.Validate` at create/rotate AND `URLPolicy.DialContext` re-resolves and re-validates at connection time and dials the validated address (closes DNS rebinding). HTTPS only, no userinfo/fragment, blocks private/loopback/link-local/multicast/CGNAT/doc/reserved ranges; no proxy; redirects not followed; 4 KiB response cap; 10s total timeout. `MAILX_WEBHOOK_ALLOW_INSECURE` relaxes all of it (dev only).
- Domain ownership: 256-bit token TXT proof; Normalize rejects wildcard/IP/IDN/`xn--`/non-ICANN/public-suffix names; pending claims by multiple tenants allowed so squatters cannot block the real owner; verified ownership unique per name.
- Outbound MIME: CRLF/header-injection rejection on every user-controlled field; Bcc never appears in headers.
- SMTP receiver: bounded line/message/connection/recipient/time limits.
- Compose: loopback-only host ports, non-root image, dev-only credentials.

## Worker / concurrency

Delivery: exactly `MAILX_WORKERS` goroutines, bounded queue capacity (Enqueue blocks at capacity = backpressure), panic recovery per job, `bookkeepingTimeout` 5s, half-lease cap on persistence timeout.
Redis queue polling (200ms..2s), not blocking primitives (multi-condition wake-up). Webhook: fan-out poll 250ms (batch 100, immediate loop when full), workers default 4, claim lease 30s, max 8 attempts, backoff 1m doubling to 1h with equal jitter, `Retry-After` honored (cap 1h). 2xx = success; 408/429/5xx/network = retry; other 4xx = terminal.

## Known limitations and technical debt (verified present at HEAD)

1. **Remote acceptance -> hard crash before local persistence** remains ambiguous: SMTP and PostgreSQL cannot share a transaction; the message may be re-sent after reclaim. No exactly-once SMTP (documented in code).
2. **DSNs are generated but never transmitted** (`worker.handleTerminalFailure` builds/serializes then discards). A terminal failure emits a `failed`/`bounced` event, but the sender gets no bounce email. Also `handleTerminalFailure` runs after outcome persistence and is not itself durable.
3. **Webhook delivery is at-least-once, no manual replay** (deferred), no global ordering guarantee across events/endpoints.
4. **Process-local retry state** (`stateStore`) survives only as a cache; durable state is authoritative on every job (v0.20-era note about memory-only state is superseded by `57ee1d0`).
5. (Resolved in v0.26) Verified-From is enforced at the API boundary. Remaining: no per-domain "require DKIM" flag (a domain requires DKIM exactly when it has an active key); no automatic key expiry or scheduled rotation; no explicit key deletion endpoint; Ed25519 and header oversigning deferred; no DKIM DNS re-check after activation.
6. (Resolved in v0.23) Readiness now checks PostgreSQL and Redis; metrics and structured logs exist. Remaining: no tracing, no log/metric shipping, metrics endpoint unauthenticated (bind to loopback/private network only), `queue_depth` is scraped live from Redis each scrape.
7. **Mixed-domain recipients rejected** (one delivery domain per message); recipient-level partial success is not modeled (RCPT is all-or-error).
8. **No inbound STARTTLS and no inbound SMTP AUTH** (outbound STARTTLS exists since v0.24); IDN/A-label domains unsupported in DNS and domain ownership. Outbound TLS gaps: no DANE/MTA-STS, so MX-to-domain binding relies on DNS; default opportunistic policy fails closed on a peer that advertises STARTTLS with an untrusted or mismatched certificate (no unverified-encryption mode); TLS facts are logs/metrics only, not persisted per attempt.
9. **FileStore write precedes DB commit** in the API path: a failed tx can leave an orphan message directory (deliberate and documented as harmless in `email_handler.go`: the reverse, a DB row without bytes, cannot happen; no reaper exists). Raw MIME lives on local disk (single-node storage).
10. **SMTP receiver is not wired to the outbox/queue**: inbound mail is stored, not relayed.
11. Attempt limits/backoff are fixed defaults (5 ops, 30m/4h), not configurable via env.
12. PostgreSQL integration tests use `MAILX_TEST_DATABASE_URL` when set, otherwise a local default DSN, and **skip** if unreachable; Redis tests **fail** if Redis is unreachable. Neither self-provisions. Validation runs must set the DSN explicitly or DB coverage is silently lost.
13. `newMux` (`internal/api/routes.go`) falls back to a zero-key `SecretBox` when no `routeServices` are passed. Production always passes a real webhook service (`api.Config.Webhooks` is required), so this is a test-scaffold footgun, not an active bug.
14. Webhook secret has a single master key and no key-versioning/rotation path for the master key.

## Superseded (do not treat as current)

- Pre-v0.26: outbound mail from any From domain was accepted (v0.21 recorded ownership but did not enforce it) and every message was sent unsigned.
- Pre-v0.24: outbound SMTP was always plaintext; the client sent MAIL FROM straight after the first EHLO and its context watcher set deadlines on the (single) connection field.
- Pre-v0.23: ad-hoc `log.Printf` logging (including full inbound message bodies/addresses in the SMTP sink), PostgreSQL-only readiness with raw error text in the 503 body, and `withRecoverMiddleware` outside the request-ID middleware.
- v0.18 worker "best-effort status reporter after Ack/Release" -> replaced in `57ee1d0` by transactional `OutcomeStore` persisted before finalization.
- v0.18 temporary `devTenantMiddleware` -> replaced by API-key `authenticateMiddleware` in v0.19.
- v0.15 `recipients UNIQUE(message_id, address)` -> `(message_id, address, header_kind)` (migration 000003).
- v0.19 partial index `idx_api_keys_tenant_active` -> superseded by 000008 tenant/created_at index.
- v0.16/v0.20 statements that retry state is process memory -> superseded by durable reconstruction (`57ee1d0`).
15. Relay credentials live in the process environment and memory for the process lifetime (Go strings cannot be zeroed); no `_FILE` secret input, no rotation without restart. Implicit-TLS relays (port 465), OAuth/XOAUTH2 and SCRAM are unsupported; one relay for all domains and tenants.
16. DKIM master key lives in the environment like the webhook key (no `_FILE` input, no re-encryption tooling for master-key rotation); losing it makes stored DKIM keys undecryptable and domains with an active key refuse to send until rekeyed.
