# Software Requirements Specification: MailX v0.23 Observability

| Field | Value |
| --- | --- |
| Document ID | SRS-mailx-v0.23-1 |
| Status | baselined |
| Author | analyst role |
| Reviewer | repository owner via supplied milestone and `assume` approval |
| Baselined on | 2026-09-21 |
| Supersedes | none |

## 1. Purpose and scope

MailX v0.23 shall expose diagnostic signals that describe existing PostgreSQL, Redis, SMTP, delivery, event, and webhook truth. It shall not create a competing lifecycle store.

Terms:

- **Log:** structured diagnostic record written to process output.
- **Metric:** aggregate bounded-cardinality numerical measurement.
- **Liveness:** whether the process HTTP loop is running.
- **Readiness:** whether required PostgreSQL and Redis dependencies answer within a bounded interval.
- **Correlation:** stable IDs linking records across subsystems.

References: Go `log/slog`; Prometheus text exposition format; existing MailX v0.22 event and delivery contracts.

## 2. Product context

MailX is a Go process containing SMTP reception, HTTP API, PostgreSQL/outbox, Redis queue, outbound delivery workers, durable lifecycle events, and webhook delivery. Operators consume stdout/stderr and an operator HTTP listener. Developers continue to use durable email/event/webhook API resources for lifecycle inspection.

Assumptions: DEC-001..DEC-007. Risks: RSK-001..RSK-003.

### Out of scope

- v0.24 STARTTLS/TLS
- distributed tracing and OpenTelemetry export
- remote log or metric shipping
- application-logs database tables
- Grafana, Loki, Tempo, Jaeger, Elasticsearch, or Prometheus server deployment
- message-content logging
- metric labels containing IDs, addresses, domains, URLs, or arbitrary errors
- changes to durable email, event, retry, or webhook truth

## 3. Functional requirements

### REQ-001 — Logging configuration

MailX shall configure one `log/slog` logger from `MAILX_LOG_LEVEL` (`debug`, `info`, `warn`, `error`) and `MAILX_LOG_FORMAT` (`json`, `text`). Invalid values shall stop startup with a configuration error.

Acceptance: each valid combination emits parseable output at or above the selected level; every invalid value returns an error before listeners start.

### REQ-002 — Structured process lifecycle logs

MailX shall emit structured startup, component stop, and process stop records containing subsystem and configured listener information without credentials or DSNs.

Acceptance: captured records contain event/subsystem fields and none of the privacy markers in NFR-001.

### REQ-003 — HTTP correlation and access records

Every API request shall receive a server-generated `X-Request-Id`. The completed-request record shall contain request ID, method, route path, status, duration in milliseconds, and authenticated tenant ID when authentication succeeded.

Acceptance: success, authentication failure, malformed input, and panic tests return/correlate the same request ID and never emit headers or bodies.

### REQ-004 — SMTP session records

Each accepted SMTP connection shall receive one process-local session ID. MailX shall record connection start/end, message acceptance, temporary sink failure, and protocol/transport failure using the session ID and generated MailX message ID where available.

Acceptance: SMTP tests observe the events and prove DATA, body, subject, sender address, and recipient address markers are absent.

### REQ-005 — Queue and dispatcher records

MailX shall record dispatcher enqueue/mark failures and worker claim/finalization transitions using job ID and message ID, without queue payload content.

Acceptance: claim, release, acknowledgement, persistence failure, lease renewal failure, terminal reclaim, and panic paths have one attributable record each.

### REQ-006 — Delivery records

MailX shall record delivery attempt completion with message ID, attempt number, bounded outcome kind, accepted flag, SMTP code when present, duration, and lifecycle decision. It shall not record remote SMTP text, recipient, domain, or raw message data.

Acceptance: success, temporary failure, and terminal failure fixtures produce the expected fields and omit prohibited markers.

### REQ-007 — Webhook records

MailX shall record webhook claim, attempt completion, retry schedule, terminal failure, lease reclaim, subscription disable, and secret rotation using tenant/subscription/event/delivery IDs and bounded outcome fields.

Acceptance: webhook success/retry/failure tests contain correlation IDs while signing secret, master key, signature, URL, and response body markers remain absent.

### REQ-008 — Aggregate metrics

MailX shall maintain in-process counters, gauges, and duration histograms for HTTP, SMTP, email delivery, queue, webhook, and process/runtime behavior using only the labels listed in the design baseline.

Acceptance: deterministic unit tests increment each metric family and a pedantic registry gathers without error.

### REQ-009 — Operator metrics endpoint

MailX shall expose Prometheus-compatible metrics at `/metrics` on a dedicated listener configured by `MAILX_OBSERVABILITY_ADDR`, separate from the developer API listener.

Acceptance: the endpoint returns HTTP 200 and a supported Prometheus content type; `/v1` routes are absent from the operator listener.

### REQ-010 — Liveness

The operator listener shall expose `/health/live` without querying PostgreSQL, Redis, DNS, SMTP, or webhook destinations.

Acceptance: liveness returns 200 while injected dependency checks fail.

### REQ-011 — Readiness

The operator listener and existing developer readiness route shall report ready only when PostgreSQL and required Redis answer within two seconds total. Failure responses shall name only bounded component identifiers and shall not include connection errors.

Acceptance: available dependencies return 200; PostgreSQL failure, Redis failure, cancellation, and timeout return 503 while liveness remains 200.

### REQ-012 — Queue depth observation

The Redis queue shall expose a read-only count of available plus claimed jobs for readiness-independent metric collection. Collection failure shall update a bounded error metric and shall not alter queue state.

Acceptance: queue tests compare depth with enqueue, claim, release, and acknowledgement transitions.

### REQ-013 — Build identity

Startup and metrics shall expose version and commit values supplied at build time, falling back to `dev` and `unknown` without inventing release identifiers.

Acceptance: injected and fallback values are both tested.

### REQ-014 — Durable inspection reuse

MailX shall retain existing email, event, delivery-attempt, and webhook-delivery APIs as the durable inspection surface; no logs table or duplicate lifecycle endpoint shall be created.

Acceptance: schema migration count remains unchanged and existing API regression tests pass.

### REQ-015 — Observability isolation

Logging and metric operations shall be local in-process operations. Their failure shall not change queue finalization, SMTP results, database state, event state, or webhook state.

Acceptance: injected observer failures leave the underlying operation result unchanged.

## 4. Non-functional requirements

### NFR-001 — Privacy

Across the test marker set of nine secret/content categories, captured logs, metrics, and health bodies shall contain zero marker occurrences.

Measurement: search captured output for API key, Authorization, webhook secret, master key, database password, Redis password, raw MIME, body, and attachment markers. Threshold: 0 occurrences.

### NFR-002 — Metric cardinality

Every metric vector shall use at most three labels, and every label value shall come from a finite set documented in the design. Threshold: 0 ID/address/domain/URL/error-string labels.

### NFR-003 — Readiness bound

One readiness request shall complete within 2.2 seconds when dependency checks block. Measurement: wall-clock test with blocking fakes.

### NFR-004 — HTTP duration units

HTTP, delivery, and webhook duration metrics shall use seconds and histogram buckets fixed at construction time. Threshold: 100% of duration metric names end in `_seconds`.

### NFR-005 — Concurrency

Metric updates and logger use shall report zero race detector findings under `go test -race -count=1 ./...`.

### NFR-006 — Baseline regression

All Go packages shall pass `go test -count=1 ./...`, `go vet ./...`, `go build ./...`, and `git diff --check`; PostgreSQL tests shall run with an explicit DSN and Redis tests with an available Redis instance.

### NFR-007 — API overhead

HTTP instrumentation shall perform zero additional database queries and zero network calls per request. Measurement: design inspection and tests using injected counters.

### NFR-008 — Response bounds

Health response bodies shall be at most 1 KiB and metric exposition shall not contain message-derived strings.

### NFR-009 — Configuration surface

v0.23 shall add no more than three environment variables: log level, log format, and operator listener address.

### NFR-010 — File organization

No new production file shall exceed 400 lines and no new production function shall exceed 120 lines without a recorded review finding.

## 5. Domain requirements

### DOM-001 — Prometheus exposition

The metrics endpoint shall be emitted by the maintained Prometheus Go client and pass its pedantic registry validation. This applies because deployments scrape Prometheus-compatible targets.

### DOM-002 — Go structured logging

Structured records shall use Go `log/slog` JSONHandler or TextHandler so level filtering and concurrent handler use follow the standard-library contract.

## 6. External interfaces

- Environment: `MAILX_LOG_LEVEL`, `MAILX_LOG_FORMAT`, `MAILX_OBSERVABILITY_ADDR`.
- Operator HTTP: `/metrics`, `/health/live`, `/health/ready` on the observability listener.
- Existing developer HTTP: current health paths remain compatible.
- Process output: line-delimited JSON in production/default configuration; key/value text when explicitly selected.

## 7. Approval

Requirements originate from the owner-supplied v0.23 specification and repository evidence. Construction may begin only after G1–G3 records pass.
