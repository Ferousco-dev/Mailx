# Decisions

- DEC-001: Rigour 3 because MailX is production-oriented email infrastructure.
- DEC-002: Potential harm includes privacy exposure, false delivery diagnosis, credential leakage, and false health signals.
- DEC-003: Treat addresses, message content, API keys, webhook secrets, and infrastructure credentials as sensitive.
- DEC-004: Use a hybrid process: gated requirements/security verification with incremental Go construction.
- DEC-005: No fixed deadline is assumed; correctness and privacy outrank speed.
- DEC-006: The user is owner and approver; automated regression evidence compensates for a single-person implementation context.
- DEC-007: Success requires end-to-end correlation, low-cardinality metrics, safe health signals, privacy tests, and all validation gates passing.

## Recovered architectural decisions (2026-09-21 historical recovery; source: Git + code, see `milestones.md` / `architecture.md`)

Process decisions above (DEC-001..DEC-007) belong to the v0.23 FLEET run and stay current for v0.23.

- DEC-008 [v0.1-v0.5]: SMTP is implemented at protocol level (raw TCP, own state machine and reply handling) rather than via a library; the project's stated purpose is to understand and own the protocol. Evidence: `09cec8f` -> `internal/smtp`, README.
- DEC-009 [v0.3]: `mail.Envelope` (MAIL FROM/RCPT TO) is separate from `mail.Message` (headers/body) because delivery uses envelope recipients; headers never control routing (hidden Bcc). Evidence: `internal/mail/model.go`.
- DEC-010 [v0.5]: MailX assigns its own message ID; the `Message-ID` header is untrusted metadata. Raw `.eml` is stored byte-exact plus derived metadata; attachments are metadata-only on load. Evidence: `internal/storage/store.go`.
- DEC-011 [v0.6]: Transport limits are finite by default (zero value selects a finite default, never "unlimited" for size). Evidence: `internal/smtp/config.go`.
- DEC-012 [v0.7]: `Accepted` means 2xx to the DATA terminator; later QUIT failure is recorded, never reverses acceptance (RFC 5321 4.1.1.10). Evidence: `smtp/client.go`, `transfer.go`, `delivery.go`.
- DEC-013 [v0.9]: DNS package only discovers; Null MX (RFC 7505) is terminal and never falls back to implicit MX; NXDOMAIN and temporary failure are distinct; ASCII-only domains until IDNA is designed. Evidence: `internal/dns/resolver.go`.
- DEC-014 [v0.10]: MX fallback only for pre-session failures; a remote answer after session engagement is authoritative to avoid duplicate work/odd DSNs. Evidence: `delivery/policy.go`.
- DEC-015 [v0.11]: Retry defaults fail safe (zero Decision = TerminalFailure); Coordinator performs exactly one operation and never waits; caller owns timing and State. Evidence: `retry/`.
- DEC-016 [v0.12]: DSNs claim only what MailX can truthfully know (omit unknown RFC 3464 fields; unconfirmed sibling recipients get a generic diagnostic); generation is separate from sending. Evidence: `bounce/`.
- DEC-017 [v0.13/v0.16]: Queue is at-least-once by design; nothing can make remote SMTP and Ack atomic. Redis holds scheduling and claim ownership only. Lua scripts make each transition atomic; polling chosen over blocking primitives. Evidence: `queue/queue.go`, `redis.go`.
- DEC-018 [v0.14]: Fixed-size worker pool, no goroutine-per-job; worker depends on narrow interfaces (Loader, Coordinator, OutcomeStore). Evidence: `worker/worker.go`.
- DEC-019 [v0.15]: PostgreSQL is durable truth; tenant_id on every table from day one; crypto-random TEXT IDs; raw MIME stays in FileStore; per-operation `delivery_attempts` with MX detail as JSONB; indexes justified by real queries + EXPLAIN tests. Evidence: migration 000001.
- DEC-020 [v0.17]: Deterministic schema setup via a one-shot `migrate` service; Redis AOF; loopback-only ports. Evidence: `compose.yaml`.
- DEC-021 [v0.18]: Transactional outbox closes the commit-then-crash-before-enqueue gap; HTTP 202 = durably accepted. Dispatcher relies on duplicate-safe Enqueue plus guarded `MarkOutboxDispatched`, no new lock primitive. Evidence: migration 000004, `dispatch/`.
- DEC-022 [v0.18]: stdlib `net/http` (no framework); public API schemas decoupled from DB/worker structs; hand-written OpenAPI with drift test; mixed-domain sends rejected before acceptance because the engine delivers per domain. Evidence: `internal/api`.
- DEC-023 [v0.19]: API key = `mx_<key_id>_<secret>`, HMAC-SHA256 verifier (+pepper), no password hashing (256-bit random secret); rotation via `expires_at` grace; identical generic 401; scopes enforced in Go and DB CHECK; no public key-creation endpoint; rows never deleted. Evidence: `internal/auth`, migration 000006.
- DEC-024 [v0.20]: Idempotency arbiter is PostgreSQL only; scoped to tenant; fingerprint over validated request; completion in the same tx as the message insert; stale-claim reclaim with fingerprint-guarded completion; replay reflects current state. Evidence: `idempotency`, migration 000007, `8ba5264`..`389c31d`.
- DEC-025 [v0.21]: Domain ownership by DNS TXT proof with strict normalization; pending claims may overlap across tenants, verified ownership is unique; verification does not imply SPF/DKIM/DMARC. Evidence: `internal/domain`, migration 000009.
- DEC-026 [57ee1d0]: Delivery outcome (attempt + status + event) persists in one transaction before queue finalization; worker consults durable state before SMTP; queue `Renew` protects live workers during DB outages; retry state rebuilt from PostgreSQL. Replaces best-effort status reporting. Evidence: `worker/processor.go`, `database/outcomes.go`, migration 000010.
- DEC-027 [v0.22]: `events` is the single event truth; webhooks are derived fan-out (separate tables, atomic fan-out, per-subscription delivery rows). Public event names are a stable mapping decoupled from internal names; `delivery_attempted` stays internal. Evidence: `database/events.go`, `webhook_fanout.go`.
- DEC-028 [v0.22]: Webhooks are at-least-once with stable event ID; consumers deduplicate; no exactly-once claim. Evidence: OpenAPI text, `UNIQUE(subscription_id,event_id)`.
- DEC-029 [v0.22]: Signing secrets need recoverable storage, so AES-256-GCM with a master key and tenant-ID AAD instead of one-way hashing; HMAC-SHA256 `v1` signature over `timestamp.body`. Evidence: `webhook/secret.go`, `signing.go`.
- DEC-030 [v0.22]: SSRF defence is enforced at dial time as well as at creation to defeat DNS rebinding; no proxies, no redirects. Evidence: `webhook/policy.go`, `client.go`.
- DEC-031 [v0.22]: Webhook delivery uses PostgreSQL leases with fencing tokens and `SKIP LOCKED`, not Redis, so notification work is independent of the email queue. Evidence: `database/webhook_deliveries.go`.
- DEC-032 [recovery]: `.ilana/` had never been committed and stopped being maintained after the initial milestones; from 2026-09-21 Ilana updates are mandatory per task (`CLAUDE.md`).
- DEC-033 [v0.23]: Observability is a leaf package; components get optional logger/metrics (nil-safe, panic-guarded) so behavior is unchanged when absent. Prometheus official client with a private pedantic registry; label values allowlisted else `other`.
- DEC-034 [v0.23]: Operator listener is separate from the developer API (`MAILX_OBSERVABILITY_ADDR`, default `:9090`, empty disables); developer `/health/ready` shares the bounded readiness checker and no longer echoes raw errors.
- DEC-035 [v0.23]: Queue depth and ping are concrete `RedisQueue` methods (interface unchanged); depth and its error counter come from one collector so a scrape is self-consistent.
- DEC-036 [v0.23]: A worker survives transient queue-claim failures (DEF-004) so dependency outages degrade readiness instead of killing the process.
- DEC-037 [v0.24]: Outbound STARTTLS is a small explicit state machine in `client_tls.go`; `raw` vs `conn` split keeps deadlines race-free across the upgrade; capabilities are nil'd when the handshake starts and only ever set from the latest EHLO.
- DEC-038 [v0.24]: Two policies only (`opportunistic` default, `required`). Default is opportunistic because RFC 3207 forbids public servers from requiring STARTTLS; it never downgrades after attempting TLS (stricter than MTAs that retry plaintext after a failed handshake).
- DEC-039 [v0.24]: Certificate verification is always on (no insecure option). Opportunistic therefore verifies too; peers with self-signed/mismatched certs are deferred until fixed or trusted via `MAILX_SMTP_TLS_CA_FILE` (extra roots added to the system pool). An unverified-encryption mode was deliberately not built.
- DEC-040 [v0.24]: All TLS-stage failures are temporary (statements about the connection, not the recipient) and the delivery engine tries the next MX for them (before MAIL FROM, same policy), reusing the existing retry model.
- DEC-041 [v0.24]: Inbound STARTTLS is deferred (needs certificate provisioning/reload); the inbound EHLO must not advertise it.
- DEC-042 [v0.24]: Test infrastructure lives in `internal/smtp/smtptest` (ephemeral in-memory CA + scriptable fake MX) so smtp, delivery-style and worker pipeline tests share one deterministic TLS server.
- DEC-043 [v0.25]: Trusted relay is an explicit `delivery.Config.Relay`; relay mode replaces MX routing entirely and never falls back to direct delivery, so an operator's "deliver through this relay" intent cannot be silently bypassed.
- DEC-044 [v0.25]: Credentials exist only inside `Config.Relay` and per-request `DeliveryRequest.Auth`; the SMTP client holds none. This makes "credentials never reach a DNS-discovered MX" structural rather than a runtime check.
- DEC-045 [v0.25]: Mechanisms PLAIN (preferred) and LOGIN only, only over verified TLS; credentials force TLS-required regardless of policy. OAuth/XOAUTH2, SCRAM, CRAM-MD5 deferred or rejected (see design doc).
- DEC-046 [v0.25]: Every auth-stage failure is a temporary attempt at the engine (existing backoff, never another MX): authentication describes relay configuration, not the message, so bouncing senders permanently for an operator error was rejected.
- DEC-047 [v0.25]: Remote AUTH reply text is discarded (code and enhanced status only) and error text is a fixed category, because servers may echo credentials.
