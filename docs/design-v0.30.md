# MailX v0.30 Design: recipient suppression management

## What a suppression is
Durable POLICY: "this tenant must not send ordinary mail to this address". It is not a delivery fact. A message that bounced stays exactly what happened; the suppression is a consequence that governs FUTURE sends. Creating or deleting one never rewrites history, retries or resurrects anything. Complaints and unsubscribes are reserved vocabulary only (see Deferred).

## Research (protocol truth vs product policy)
- RFC 3463 (read from the RFC text): X.1.1 "Bad destination mailbox address" (the mailbox does not exist; "only useful for permanent failures"), X.1.6 "Destination mailbox has moved, no forwarding address" (permanent), X.1.2 bad destination SYSTEM (domain), X.1.3 mailbox syntax, X.2.1 mailbox disabled and X.2.2 mailbox full (can recover), X.7.x delivery not authorized (policy). RFC 5321 2.4: local-part case MUST be preserved by SMTP implementations in transport, but exploiting case sensitivity is "discouraged".
- Providers (reference only, not cloned): Amazon SES puts only HARD bounces on its account-level suppression list; SendGrid suppresses 5.1.1 invalid-recipient bounces and retries soft bounces; Postmark suppresses hard bounces and complaints, lets hard bounces be reactivated manually, and does not let complaint suppressions be reactivated. MailX policy: suppress only recipient-specific permanent mailbox failures, never soft/temporary ones, and let the tenant remove any entry.

## Scope and key
Tenant-scoped: `UNIQUE (tenant_id, email)`. Another tenant's bounce never influences a tenant's policy; cross-tenant ids are 404. One canonical function, `suppression.Normalize`, is used by the API, the worker and the store (the store re-normalizes, so no caller can create a key another caller's lookup would miss).
- Removes surrounding whitespace and ONE `<...>` pair (the worker sees bracket-form envelope recipients); ONE trailing root dot on the domain; ASCII only (no SMTPUTF8/IDN recipients exist anywhere in MailX); plain dot-atom local part (no quoted forms); ASCII LDH domain labels, no IP literals or IP-looking names.
- The LOCAL PART IS LOWER-CASED FOR THE KEY ONLY, and MailX still transmits the address exactly as given. Justification: for a deny list the safe direction is to treat case variants as one recipient, because the alternative lets a suppressed recipient receive mail by changing case, while the only cost is a hypothetical pair of mailboxes differing only by case, which RFC 5321 2.4 itself discourages.
- NO provider-specific rules: dots and `+tag` are significant and never folded (`john.smith@gmail.com` and `johnsmith@gmail.com` are different keys).
- Consequence, enforced at acceptance: every recipient MailX accepts must be keyable (422 `invalid_recipient` otherwise), so nothing accepted can be un-suppressible. Older messages with unkeyable recipients (accepted before v0.30) stay deliverable and can never have been suppressed.

## Vocabulary
`reason`: manual | hard_bounce | complaint | unsubscribe. `source`: api | delivery | feedback. The API can create ONLY `manual`/`api`; hard bounces are created by delivery outcomes; complaint/unsubscribe/feedback are in the schema CHECK so v0.32 needs no schema change, but nothing produces them and the API refuses them (a caller cannot forge system facts). Reason and source are separate: WHY vs HOW.

## Automatic hard-bounce suppression (`suppression.QualifiesHardBounce`)
Structured fields only, never message text. A failed attempt qualifies iff ALL hold: permanent; not accepted; stage `rcpt_to` (the recipient command itself was rejected); a named recipient; SMTP code 5xx; enhanced status `5.1.1` or `5.1.6`. NOT suppressed: any 4xx/temporary failure (and retries exhausted), failures at MAIL FROM/DATA/AUTH/TLS stages, 5.7.x policy or authentication (SPF/DKIM/DMARC/blocklist), 5.1.2 and 5.1.10 (domain-level), 5.1.3 (syntax), 5.2.x (full/disabled can recover), a bare 550 with no enhanced code (also used for policy/blocklist), DNS failures, null MX. The suppression is written in the SAME transaction as the attempt, message status and event, so after a crash the durable facts never say "bounced" while the next message to the dead address is still allowed; `ON CONFLICT DO NOTHING` makes concurrent workers converge on one row and the first writer's facts win. Tested through the real production adapter and database with every class above.

## Enforcement points
1. Acceptance (`POST /v1/emails`, after authorization/signing, before anything is stored): invalid recipient -> 422; ALL recipients suppressed -> 422 `all_recipients_suppressed`, nothing stored, the Idempotency-Key is NOT consumed; partially suppressed -> accepted; a failing lookup refuses the request (never "assume not suppressed"). Fast developer feedback only; it is not the guarantee.
2. Delivery (`worker.enforceSuppression`), once per attempt including retries, BEFORE the coordinator and any transport call (so before MAIL FROM/RCPT/DATA, direct or relay). One indexed query (`SuppressedForMessage`, resolves the message's tenant in the same query). Suppressed recipients are removed from the envelope: a, b(suppressed), c => SMTP for a and c only, and zero SMTP connections when everything is suppressed (proved by counting real connections at a fake MX, direct and relay).
The guarantee is exactly: a recipient suppressed when the worker performs its check gets no SMTP attempt.

## Consistency boundary (truthful)
A suppression created after the worker's check does not revoke an attempt already underway; MailX never holds a database lock across network I/O to pretend otherwise (tested: the in-flight attempt completes and is recorded, and the very next message is suppressed). Once the remote server accepts DATA, `Accepted=true` is immutable history: recording suppression for a delivered message is refused (`ErrMessageTerminal`), and recipient rows of delivered messages are untouched.

## Failure policy
Unknown suppression state never becomes "not suppressed, send". If the lookup or the record write fails, the job is deferred 15 s (`release`), no SMTP happens, no attempt is recorded, retry counters do not move and the message cannot fail because PostgreSQL was briefly unavailable. Cancellation during a lookup stops the pool with nothing sent, acked or lost. Tested for lookup failure, record failure, ack failure and cancellation.

## Recipient and message state
Recipient rows gain `suppressed` (no SMTP attempt named it; not failed, not bounced). Message status gains terminal `suppressed` (every recipient suppressed; no attempt). Deterministic aggregation: all suppressed -> `suppressed`; suppressed + rest delivered -> `delivered`; suppressed + rest retrying -> `retrying` (later attempts skip the suppressed ones; if all become suppressed between retries the message becomes `suppressed`, from `retrying`); suppressed + rest permanently failing -> `failed`. Pre-existing limitation kept: RCPT is all-or-error, so one bad recipient still fails the whole message; v0.30 stops the repeat by suppressing that recipient for future messages.

## Crash safety and ordering
`RecordSuppressedRecipients` (recipient rows + one event + the terminal status) is one short transaction; the queue job is acked only AFTER it commits. If the ack then fails, the reclaimed job finds the durable terminal state and acks again with no SMTP; replays are idempotent (at most one `suppressed` event per message via a partial unique index).

## Unsuppression
`DELETE` is a HARD delete: future sends may be attempted again; nothing is re-queued or re-sent; a message that ended `suppressed` stays terminal and history/events are untouched (tested end to end). Hard vs soft delete: hard keeps the uniqueness rule and the hot query trivial (one row per key, no `deleted_at` predicate in a safety-critical query) and matches "removed means gone"; the cost is no built-in audit trail for removals (the log/metrics record only bounded counts) and it is recorded as a risk. Creating an existing address returns the existing entry (200), so retries and races are safe; this is separate from the HTTP Idempotency-Key, which POST /v1/suppressions does not need.

## API
`POST/GET /v1/suppressions`, `GET/DELETE /v1/suppressions/{id}`; new scopes `suppressions:read` / `suppressions:write` (never implied by other scopes; the api_keys CHECK is extended). Keyset pagination on (created_at, id) like the other list endpoints, optional exact `email` filter. Structured errors: `invalid_email`, `invalid_reason`, `suppression_not_found`, `all_recipients_suppressed`, `invalid_recipient`. OpenAPI documents normalization, duplicate behavior, unsuppression semantics, recipient-level behavior, hard-bounce rules and that temporary failures never suppress.

## Events and webhooks
One lifecycle event, `suppressed` (public `email.suppressed`), at most once per message, payload = counts only (no addresses). Suppression-list mutations (created/deleted) are deliberately NOT events (no event explosion; the list is queryable). It flows through the existing durable fan-out (stable ids, at-least-once); a webhook failure never changes suppression truth (tested). The subscription CHECK and `webhook.EventTypes` accept it.

## Schema, indexes, performance
Migration 000013 (up and safe down): `suppressions` (id, tenant_id, email, reason, source, message_id, smtp_code, enhanced_status, created_at) with CHECKs; constraints/values extended on api_keys, messages, recipients, events, webhook_subscriptions. Two indexes only: `uq_suppressions_tenant_email UNIQUE (tenant_id, email)` serves both uniqueness/concurrency control AND the hot lookup (no redundant second index); `idx_suppressions_tenant_created (tenant_id, created_at DESC, id DESC)` serves the keyset list. Measured with EXPLAIN (ANALYZE, BUFFERS) on 120,000 rows over two tenants: recipient lookup = Index Only Scan on the unique index, 0.031 ms, 14 buffers; the worker's message->tenant->suppressions join 0.016 ms; the list = index-ordered Index Only Scan with LIMIT, no sort node, 0.014 ms. End-to-end benchmark (including the Postgres round trip): about 0.36 ms per delivery attempt. Write cost: each entry stores two small index entries. Upgrade over an existing v0.29 database and the downgrade with new-vocabulary rows are tested, and verified on a copy of a real dev database in the Docker image.
No Redis cache: PostgreSQL is the only source of truth; a cache would add invalidation and unsuppress-race semantics for no measured need.

## Observability and privacy
`mailx_suppression_checks_total{result: clear|partial|all|error}` and `mailx_suppression_writes_total{reason,source}` (allowlisted; `other` otherwise). No address, domain, tenant or message id in any label; logs carry job/message ids only. Health is unchanged (no external dependency).

## Deferred / limitations
Complaint ingestion and feedback loops (v0.32); unsubscribe and any subscription/contact model; per-recipient delivered/failed states (RCPT all-or-error); bulk import/export; suppression-list audit trail; global/platform (cross-tenant) abuse suppression (v0.31); case variants are one key by design (documented trade-off).
