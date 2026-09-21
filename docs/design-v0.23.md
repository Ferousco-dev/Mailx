# MailX v0.23 Design: Observability (DES-001..DES-008)

Status: baselined 2026-09-21. Traces to `docs/srs.md`. Observability only OBSERVES PostgreSQL/Redis/SMTP truth; it never owns lifecycle state.

## Modules
- **DES-001 `internal/buildinfo`**: `Version`/`Commit` vars set via `-ldflags -X`; fallbacks `dev`/`unknown`.
- **DES-002 `internal/observability/logging.go`**: `NewLogger(w, level, format)` -> `*slog.Logger` (JSON default, text optional; invalid -> error). Handler wrapped so a panicking sink never propagates (REQ-015). `Discard()` logger for defaults.
- **DES-003 `metrics.go`**: custom `prometheus.Registry` (no global), client_golang collectors only. All methods are nil-receiver safe and panic-guarded. Label values pass through allowlists (unknown -> `other`).
- **DES-004 `operator.go`**: separate listener `MAILX_OBSERVABILITY_ADDR` (default `:9090`, empty disables): `/metrics`, `/health/live`, `/health/ready`. No `/v1`.
- **DES-005 `health.go`**: `Readiness{PostgreSQL, Redis}` runs checks concurrently under one 2 s context; body `{"status":"ready|not_ready","failed":["postgres"]}` (<1 KiB, bounded names only). Liveness never calls dependencies. Developer `/health/ready` reuses the same checker (compatible: 200/503 + `status`).
- **DES-006 Hooks**: `smtp.Config.Observer` (session/message events, `Session.ID`); `worker.WithLogger/WithMetrics`; `dispatch.WithLogger/WithMetrics`; `webhook.WorkerPool`/`Service` logger+metrics; `api.Config.Logger/Metrics`. Components default to a discard logger and nil metrics, so existing behavior is unchanged.
- **DES-007 `RedisQueue.Ping/Depth`**: concrete read-only methods (ZCARD available + claimed); `queue.Queue` interface unchanged.
- **DES-008 Correlation**: request_id (server-generated), tenant_id (post-auth), message_id, job_id, attempt (delivery attempt number), event_id, webhook_delivery_id, subscription_id, smtp session_id (process-local, random, no security meaning).

## Log fields (never: bodies, MIME, addresses, domains, URLs, secrets, signatures, remote text, error strings from remote)
HTTP `http_request`: request_id, method, route (matched pattern), status, duration_ms, tenant_id?. Panic: `http_panic` request_id only.
SMTP: `smtp_session_start/end` session_id, duration_ms, result; `smtp_message_stored` session_id, message_id; `smtp_message_rejected` session_id, reason; `smtp_storage_failed`.
Worker: `queue_claimed`, `queue_acked`, `queue_released`, `outcome_persist_failed`, `claim_renew_failed`, `terminal_reclaim`, `worker_panic`, `delivery_outcome` (job_id, message_id, attempt, kind, decision, accepted, smtp_code, duration_ms).
Webhook: `webhook_claimed`, `webhook_attempt` (outcome, response_code, error_category, delivery_id, subscription_id, event_id, tenant_id, attempt), `webhook_reclaimed`, `webhook_subscription_disabled`, `webhook_secret_rotated`.

## Metrics (namespace `mailx`, <=3 labels, finite sets)
| Metric | Labels (values) |
| --- | --- |
| `mailx_build_info` | version, commit |
| `mailx_http_requests_total` | method (GET POST PUT PATCH DELETE other), route (registered patterns + `unmatched`), status_class (1xx..5xx) |
| `mailx_http_request_duration_seconds` | method, route |
| `mailx_smtp_sessions_total` | result (completed failed rejected) |
| `mailx_smtp_active_sessions` | none |
| `mailx_smtp_messages_total` | result (accepted rejected temporary_failure) |
| `mailx_delivery_attempts_total` | kind (9 delivery kinds + other), decision (retry terminal_success terminal_failure) |
| `mailx_delivery_attempt_duration_seconds` | decision |
| `mailx_queue_operations_total` | operation (enqueue claim ack release renew persist), result (ok error) |
| `mailx_queue_depth` / `mailx_queue_depth_errors_total` | none |
| `mailx_webhook_attempts_total` | outcome (succeeded retrying failed) |
| `mailx_webhook_attempt_duration_seconds` | outcome |
| Go + process collectors | standard |

## Configuration
Exactly three new env vars: `MAILX_LOG_LEVEL` (debug|info|warn|error, default info), `MAILX_LOG_FORMAT` (json|text, default json), `MAILX_OBSERVABILITY_ADDR` (default `:9090`). Compose publishes it on 127.0.0.1 only. No migrations, no logs table.
