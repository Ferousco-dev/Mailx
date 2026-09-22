# MailX v0.32 — Outbound Feedback (Bounces & Complaints)

Scope (CR-015): asynchronous DSN/bounce feedback and complaint feedback about mail MailX previously sent,
correlated back to the message/recipient, with conservative auto-suppression. NOT general inbound email:
no mailboxes, IMAP/POP3, arbitrary inbound routing, or inbound webhooks.

## Critical invariant

SMTP transport truth (`delivery_attempts`, `messages.status`) is never rewritten by later feedback. Feedback
lands on `recipients.feedback_status`/`feedback_at` — a separate summary, guarded so re-delivery of the same
semantic outcome never re-fires suppression or events.

## Ingestion boundary

`POST /internal/feedback` — not `/v1`, not tenant-authenticated. Requires an operator bearer token
(`MAILX_FEEDBACK_INGEST_TOKEN`, constant-time compared). Body: `{type: "dsn"|"complaint", message_id or
correlation_token, raw (base64, dsn), recipient (complaint)}`. The operator runs their own bounce-mailbox
relay or provider webhook adapter and forwards here; MailX does not operate a public bounce-receiving MTA.

## Correlation

`feedback.Correlator`: HMAC-SHA256 token over the message's existing 128-bit crypto-random ID. A bare
`message_id` is also accepted, safe only because the whole route requires the operator credential; tenant is
always re-derived from the message row server-side, never from the request. VERP Return-Path
(`bounce+<token>@bounce-domain`) was evaluated and deferred — it would require a MAIL FROM change and new DNS
(a dedicated bounce domain), which this milestone does not introduce (RSK-036).

## Parsing & classification

`internal/feedback.ParseDSN`: RFC 3464 multipart/report -> message/delivery-status groups. Bounded (256 KiB,
16 groups), fuzzed, never panics. `Classify`: Action=failed + status class 5 -> bounce_permanent; Action=delayed
+ class 4 -> bounce_temporary; anything else -> bounce_unknown. Only `suppression.QualifiesAsyncHardBounce`
(5.1.1/5.1.6, mirroring v0.30) suppresses a permanent bounce; complaints always suppress.

## Persistence

`database.ProcessFeedback`, one transaction: feedback history row (dedup by `UNIQUE(message_id, recipient_id,
sha256(raw))`) -> recipient transition (`feedback_status IS DISTINCT FROM`) -> suppression (reused v0.30 table)
-> event (`EventBounced` reused, new `EventComplained`) -> existing webhook fan-out. Never depends on webhook
success.

## Deferred

Bounded metrics for ingestion (RSK-035), VERP Return-Path (RSK-036), provider-specific complaint adapters
(RSK-037). RSK-029 (RCPT all-or-error) unchanged.
