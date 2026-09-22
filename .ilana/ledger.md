# MailX v0.23 Ìlànà Ledger

## 2026-09-21 | BOOT | conductor
Mode: FLEET. Process style: hybrid. Rigour: 3.
Scope: v0.23 structured logs, correlation, metrics, and health/readiness only. v0.24 excluded.
Intake: user directed Ìlànà to assume defaults; assumptions recorded as DEC-001..DEC-007.

## 2026-09-21 | G0 | conductor | GATE PASS
Evidence:
- milestone request defines purpose, boundaries, security constraints, validation, and completion criteria
- privacy/harm assumptions recorded in `.ilana/decisions.md`
- risks RSK-001..RSK-003 recorded in `.ilana/risks.md`
- ethics finding ETH-001 recorded with no halt
Decision: enter phase 01 requirements.

HANDOFF
  from: conductor
  to: analyst
  gate: G0 PASSED
  produced: .ilana/gates/G0.md, .ilana/decisions.md, .ilana/risks.md, .ilana/ethics.md
  open: none
  next: audit repository evidence and baseline testable v0.23 requirements

## 2026-09-21 | RECOVERY | documentarian | CR-002
Ìlànà had not been updated during MailX v0.1-v0.22 and `.ilana/` had never been committed (untracked; only the v0.23 BOOT/G0 artifacts existed).
Recovered MailX history and current architecture from Git (50 commits, `1218cf8`..`481da4a`) and code at HEAD `481da4a`.
Produced: `.ilana/milestones.md` (v0.1-v0.22 with evidence grades), `.ilana/architecture.md` (current architecture, 18 invariants, security, limitations, superseded designs),
DEC-008..DEC-032, RSK-004..RSK-011, DEF-001..DEF-003, MET-002..MET-006, CR-002, `state.json.mailx`. Existing v0.23 records (DEC-001..007, RSK-001..003, ETH-001, G0, SRS) preserved unchanged.
Permanent rule: `CLAUDE.md` (Ilana MUST be updated as part of every MailX task).
Uncertain/unavailable: v0.1-v0.6 numbering is inferred; v0.3/v0.4 split, and the version owning `3581def`/`57ee1d0`, are not recorded in the repo.
Roadmap (verified): v0.22 COMPLETE; v0.23 Logs/Observability NEXT (planning only, no code); v0.24 not started.
Not run: the Go test suite (documentation-only task).

## 2026-09-21 | G1 | analyst | GATE PASS
Evidence: SRS REQ-001..015, NFR-001..010, DOM-001..002 baselined (`docs/srs.md`); traceability rows exist for all; elicitation plan present; G0 passed.
Working tree verified: only `.ilana/`, `docs/`, `CLAUDE.md` untracked; no v0.23 production code; `origin/Feranmi_works` = HEAD `481da4a`.
Decision: enter phase 02.

## 2026-09-21 | G2 | architect | GATE PASS
Evidence: `docs/design-v0.23.md` DES-001..DES-008; components observe only (PostgreSQL/Redis remain truth); no queue.Queue interface change; env surface = 3 vars; no migrations.

## 2026-09-21 | G3 | interaction-designer | GATE PASS
Evidence: operator interface specified in `docs/design-v0.23.md` (endpoints, health bodies <1 KiB, bounded component names, log field contract, metric label sets).
Decision: enter phase 04 construction.

## 2026-09-21 | G4 | constructor | GATE PASS
Evidence: `internal/observability`, `internal/buildinfo`, hooks in smtp/worker/dispatch/webhook/api/queue, `cmd/mailx/observe.go`; 3 new env vars; no migrations; new production files all < 400 lines. Prometheus client_golang v1.24.1 (+ procfs, required for Linux builds; found when the Linux build failed).
Coding-standard note: existing `log.Printf` uses in cmd/ replaced by slog; SMTP sink no longer logs message content.

## 2026-09-21 | G5 | verifier | GATE PASS
Evidence: `gofmt` clean, `go vet`, `go build` (darwin + linux/amd64 + linux/arm64), `go test -count=1 ./...` and `go test -race -count=1 ./...` with explicit MAILX_TEST_DATABASE_URL and live Redis: all packages pass. New tests in observability, buildinfo, smtp, worker, dispatch, webhook, api, queue, cmd.
Docker Compose e2e (host-built linux/arm64 binary in an alpine image because the in-Docker `go mod download` stalled on network; repo Dockerfile updated with VERSION/COMMIT build args but not exercised end to end): normal live/ready/metrics; `/v1` absent on operator listener; PostgreSQL down => live 200, ready 503 `["postgres"]`, metrics 200; recovery => ready 200; Redis down => live 200, ready 503 `["redis"]`, `queue_depth_errors_total` increments; recovery => ready 200. Real API email, unauthenticated request and inbound SMTP message with marker strings: 0 markers in logs/metrics.
DEF-004 found and fixed during e2e (below).

## 2026-09-21 | G6-G8 | release-manager | GATE PASS (local)
v0.23 complete pending commit. Not pushed. Known limits recorded in `architecture.md`. README redesign and `assets/readme/` were pre-existing user edits and are not part of this change.

## 2026-09-21 | REVIEW | constructor | CR-004
Greptile reviewed PR #14 (3 findings, DEF-005..DEF-007), all fixed with regression tests; `go vet`, `go build` (darwin, linux), `go test -race -count=1 ./...` with explicit DSN pass.
Note: architecture.md limitation 6 remains accurate; the SMTP-only mode now also serves the operator listener.

## 2026-09-21 | G0-G3 | conductor | v0.24 PLAN
Scope CR-005: outbound STARTTLS only. Reality check: HEAD was `a8849e0`, not the `4148ad6` in the request. RFC 3207 read (STARTTLS keyword, 220/454/501, state reset, EHLO after TLS, public servers must not require it). Design `docs/design-v0.24.md`. No migration needed.

## 2026-09-21 | G4 | constructor | GATE PASS
`internal/smtp/client_tls.go` (307 lines), `client.go` refactor (hello/secure, raw/conn), stages, transfer TLS info, delivery fallback, `mailx_smtp_tls_sessions_total`, worker tls_* log fields, `cmd/mailx/tlsconfig.go`, Dockerfile ca-certificates, `.env.example`/compose. No InsecureSkipVerify anywhere in production code.

## 2026-09-21 | G5 | verifier | GATE PASS
fmt, vet, builds (darwin, linux/amd64, linux/arm64), `go test -count=1 ./...` and `-race` with explicit PostgreSQL DSN and Redis: all pass, 0 skipped. TLS matrix A-M covered (client, delivery, retry, worker pipeline, observability privacy, cmd config); concurrency test repeated 15x and TLS subset 10x under -race, no leaks. Real Dockerfile build succeeded (exit 0); Compose e2e: health/readiness normal, PostgreSQL and Redis outage and recovery, invalid policy rejected at startup, `required` starts. DEF-008 found and fixed.

## 2026-09-21 | G6-G8 | release-manager | GATE PASS (local)
v0.24 complete pending commit. Not pushed. Next: v0.25 SMTP AUTH (not started).

## 2026-09-21 | G0-G3 | conductor | v0.25 PLAN
Scope CR-006: outbound SMTP AUTH to a trusted relay only. Baseline at `d0cbda6` green (fmt, vet, build, diff-check, test, race with real PostgreSQL/Redis). RFC 4954 read (mechanism list may change after STARTTLS; 535 permanent, 454 temporary; plaintext mechanisms need TLS; PLAIN over TLS mandatory; initial-response line limit). Design `docs/design-v0.25.md`. No migration.

## 2026-09-21 | G4 | constructor | GATE PASS
`internal/smtp/client_auth.go`, `client.go`/`client_tls.go` integration, `delivery.Config.Relay` + `Result.Transport`, `transfer` Auth plumbing, `mailx_smtp_auth_attempts_total`, worker log fields, `cmd/mailx/relayconfig.go`, `smtptest` AUTH scripting. No InsecureSkipVerify; no credential in any print path.

## 2026-09-21 | G5 | verifier | GATE PASS
See final validation entry appended below when the full suite, Docker build and Compose checks complete.

## 2026-09-21 | G5 (final) | verifier | GATE PASS
fmt, vet, builds (darwin, linux/amd64, linux/arm64), `go test -count=1 ./...` and `-race` with explicit PostgreSQL DSN and Redis: all pass, 0 skipped. AUTH subset and mixed-failure concurrency repeated 10x under -race with no leaks. Real Dockerfile build succeeded (exit 0). Compose: default start direct transport; relay config errors (password missing, host missing, bad port) fail startup naming variables only; a valid relay config starts and a secret marker never appears in container logs; liveness and readiness stay 200 with an unreachable relay; PostgreSQL and Redis outage/recovery unchanged. Security greps: no InsecureSkipVerify, no credential print path, no markers outside tests.

## 2026-09-21 | G6-G8 | release-manager | GATE PASS (local)
v0.25 complete pending commit. Not pushed (push planned after review). Next: v0.26 DKIM (not started). DEF-009 was caught before commit.

## 2026-09-21 | G0-G3 | conductor | v0.26 PLAN
Scope CR-007: DKIM signing, key lifecycle, verified-From. Baseline at `631836c` green (fmt, vet, build, diff-check, test, race with real PostgreSQL/Redis). RFC 6376 and RFC 8301 read (relaxed canonicalization, From must be signed, hash construction, rsa-sha1 forbidden, 2048-bit recommended). Design `docs/design-v0.26.md`; migration 000012 justified (key storage and lifecycle constraints need durable, tenant-safe schema).

## 2026-09-21 | G4 | constructor | GATE PASS
`internal/dkim` (keys, canon, sign, service), `internal/secretbox`, migration 000012, `database/dkim_keys.go`, sender authorization + in-transaction recheck, API handlers/OpenAPI, cmd wiring, metric. No InsecureSkipVerify, no weak algorithm, no math/rand, no plaintext key path.

## 2026-09-21 | G5 | verifier | GATE PASS
fmt, vet, builds (darwin, linux/amd64, linux/arm64), `go test -count=1 ./...` and `-race` with explicit PostgreSQL DSN and Redis: all pass, 0 skipped. Targeted DKIM/authorization/concurrency/rotation/deletion/delivery subset repeated 8x under -race. Independent DKIM verification (go-msgauth) of text, html, alternative, multipart/mixed+attachment; mutation and canonicalization tests; DB plaintext-marker inspection; capturing MX proves stored = transmitted bytes for direct and relay. Real Dockerfile build exit 0; Compose: migration 000012 applied, verified-From 403/202, DKIM create returns a pending TXT record with no private material, ciphertext row inspected, master-key validation (missing, malformed, same-as-webhook) fails startup naming variables only, valid key starts.

## 2026-09-21 | G6-G8 | release-manager | GATE PASS (local)
v0.26 complete pending commit. Not pushed. Next: v0.27 SPF (not started). DEF-001 and RSK-006 closed.

## 2026-09-21 | REVIEW | constructor | CR-008
Greptile reviewed PR #15 (3 findings, DEF-011..DEF-013), all fixed with regression tests; key generation is now preceded by the pending check and capped at 2 concurrent generations.

## 2026-09-21 | G0-G3 | conductor | v0.27 PLAN
Scope CR-009: SPF sending-authorization guidance. Baseline at `48939d0` green. RFC 7208 identity/evaluation rules applied (MAIL FROM vs HELO, single record, `all` ends evaluation, 10-lookup limit). Repository audit: MAIL FROM = From address, HELO = `mailx.local`. Design `docs/design-v0.27.md`. No migration: DNS is source of truth (DEC-059). Sending is not blocked by SPF (DEC-054).

## 2026-09-21 | G4 | constructor | GATE PASS
`internal/spf` (record, sending, service), API `spf_handler.go` + routes + OpenAPI, `cmd/mailx/spfconfig.go`, metric `mailx_spf_verifications_total`, compose/.env.example variables (empty by default, no fake IPs).

## 2026-09-21 | G5 | verifier | GATE PASS
fmt, vet, builds (darwin, linux/amd64), `go test -count=1 ./...` and `-race` with real PostgreSQL/Redis: all pass, 0 skipped (1229 passes under race). Parser fuzzed (FuzzParse, 50 s, no findings). SPF suite repeated 15x and API SPF tests 8x under -race. Real `docker compose build` exit 0; Compose runtime: default start healthy (live/ready), `MAILX_SENDING_IPS=127.0.0.1` refuses startup naming only the variable, `MAILX_SENDING_IPS=8.8.8.8` yields guidance record `v=spf1 ip4:8.8.8.8 ~all`, verify 409 on unverified, 404 unknown, health unaffected. No defects found in production code.

## 2026-09-21 | G6-G8 | release-manager | GATE PASS (local)
v0.27 complete pending commit. Not pushed. Next: v0.28 DMARC (not started).


## 2026-09-21 | G0-G3 | conductor | v0.28 PLAN
Scope CR-010: DMARC policy, alignment and readiness. Baseline at `cc8c178` green (23 packages, plain and race). RFC 9989 identified as the current standard (RFC 7489 obsolete). Source audit: MAIL FROM = From address, d= = From domain, HELO `mailx.local`. Real domain `appmd.dev` already publishes `v=DMARC1; p=none;` (Gmail reported dmarc=pass), used as a live existing-record case. Design `docs/design-v0.28.md`. No migration (DEC-065).

## 2026-09-21 | G4 | constructor | GATE PASS
`internal/dmarc` (record, align, service), API `dmarc_handler.go` + routes + OpenAPI, adapters in `cmd/mailx/dmarcconfig.go`, metric `mailx_dmarc_verifications_total`. No new config.

## 2026-09-21 | G5 | verifier | GATE PASS
fmt, vet, builds (darwin, linux/amd64), `go test -count=1 ./...` and `-race` with real PostgreSQL/Redis: all pass, 0 skipped (1293 passes under race). Parser fuzzed 30 s, no findings; dmarc suite 12x and API DMARC tests 6x under -race. Real `docker compose build` exit 0; Compose runtime against real `appmd.dev` DNS: existing `p=none;` preserved, DKIM and SPF paths ready, readiness ready, receiver_result not_observed; 409 for an unverified domain, 404 unknown; health unaffected; metric exposed. One test-authoring correction (MAIL FROM keeps brackets/case); no production defects.

## 2026-09-21 | G6-G8 | release-manager | GATE PASS (local)
v0.28 complete pending commit. Not pushed. RSK-020/RSK-021 (egress IP correctness, `mailx.local` HELO) remain OPEN. Next: v0.29 public SMTP identity (not started).

## 2026-09-21 | REVIEW | constructor | CR-011 v0.28 correction
Independent review found the DMARC Organizational Domain used the PSL (DEF-015, RSK-022). RFC 9989 sections 4.10, 4.10.1 and 4.10.2 re-read from the RFC text. Replaced by one bounded DNS Tree Walk that yields policy and Organizational Domain (`internal/dmarc/treewalk.go`, `align.go`); psd=y/psd=n implemented; multiple records discarded with the walk continuing yet reported as conflict; DNS errors fail safe (unknown, never a substituted parent policy). Evidence: RFC worked examples reproduced, PSL-vs-walk difference tests, policy/organization consistency test over 400 pseudo-random zones, walk bounded to 8 queries (RFC's 12-label example), parser and walk-input fuzzing, concurrency with psd cases, source scan forbidding PSL/suffix matching. Full suite plain and -race green with real PostgreSQL/Redis (1321 passes, 0 skipped), Docker build exit 0, read-only Compose run against real appmd.dev preserved its `p=none;` policy. RSK-022 closed; RSK-020, RSK-021, RSK-023 remain OPEN. No API shape change. A pre-existing test flake (DEF-016, a raw-substring assertion over the whole metrics dump) surfaced under -race and was fixed; `service.go` was split by responsibility into `service.go` (I/O) and `readiness.go` (pure model).

## 2026-09-21 | DECISION | conductor | deployment target
User chose an AWS Lightsail $7/month instance as the first deployment target (DEC-069); RSK-024 and RSK-025 recorded; the hosted-database question (Aiven free PostgreSQL) is open. Code fact verified: hosted TLS/password Redis is unsupported (architecture limitation 19). No code changed.

## 2026-09-21 | DECISION | conductor | single-host deployment
User approved DEC-070: everything on the one Lightsail $7 host, backups from day one, managed PostgreSQL only later. The earlier open hosted-database question in DEC-069 is closed. No code changed; the tuning, swap and backup steps are not yet written or implemented.

## 2026-09-21 | G0-G3 | conductor | v0.29 PLAN
Scope CR-012: public SMTP identity, PTR/rDNS readiness, EHLO and Message-ID. Baseline at `5d60bc4` green (24 packages, plain and race). Standards read from RFC text (5321 4.1.1.1/4.1.3, 7208 2.3/2.4, 1912 2.1, 5322 3.6.4) plus Gmail sender guidance (receiver practice, kept separate from RFC requirements). Audit found `mailx.local` in 4 production places (client EHLO, two Message-ID constructions, DSN Reporting-MTA). Design `docs/design-v0.29.md`; no migration.

## 2026-09-21 | G4 | constructor | GATE PASS
`internal/smtpidentity` (hostname validation, PTR/FCrDNS readiness), `cmd/mailx/smtpidentity.go` (modes, `check-smtp-identity`), Message-ID helper and `api.Config.MessageIDDomain`, worker Reporting-MTA wiring, compose/.env.example.

## 2026-09-21 | G5 | verifier | GATE PASS
fmt, vet, builds (darwin, linux/amd64), `go test -count=1 ./...` and `-race` with real PostgreSQL/Redis: all pass, 0 skipped (1392 passes under race). Hostname, PTR (A-L) and multi-IP matrices, failure injection, concurrency, three fuzzers, byte-capture EHLO tests (pre-TLS, post-TLS, HELO fallback, relay), Message-ID with independent DKIM verification, import boundaries. Real `docker compose build` exit 0; Compose runtime: local mode healthy, public mode healthy, IPs without hostname fails startup with a clear message, invalid hostname fails without echoing the value, `check-smtp-identity` works in the image; read-only live DNS check of the laptop IP reported `missing_ptr` (truthful). The user's already-running container was left untouched. RSK-021 closed; RSK-020 and RSK-023 remain OPEN.

## 2026-09-21 | G6-G8 | release-manager | GATE PASS (local)
v0.29 complete pending commit. Not pushed. OPERATIONAL NOTE: recreating the compose service with the existing `.env` (which declares `MAILX_SENDING_IPS`) now requires `MAILX_SMTP_HOSTNAME`. Next: v0.30 suppression (not started); the first controlled Internet-delivery test is a separate explicit action.

## 2026-09-21 | DECISION | conductor | low-memory PostgreSQL profile
User asked for the tested PostgreSQL settings as an opt-in profile (DEC-077). Added `compose.low-memory.yaml` (durability pinned on), `docs/low-memory-deployment.md`, startup database-settings validation (`database.DB.ServerSettings`, `cmd/mailx/dbsettings.go`) and tests. Evidence: Compose applies the exact settings (checked in `pg_settings`); MailX is silent against the profile and warns four ways against a weakened server; full suite green against a `max_connections=20` server (six full runs, no connection errors). One earlier first-run of the suite against a fresh tuned database reported 5 non-ok lines that could not be reproduced in six later runs (including fresh-database first runs with and without the tuning); cause unknown, no evidence it is related to the tuning. The default `compose.yaml` is unchanged.

## 2026-09-21 | G0-G3 | conductor | v0.30 PLAN
Scope CR-013: recipient suppression management. Baseline at `0b215ba` green (25 packages, plain and race). Traced one recipient: POST /v1/emails -> InsertMessage (recipients rows, outbox, queued event) -> queue -> worker.processOne -> coordinator/engine -> SMTP -> `PersistDeliveryOutcome` (attempt + status + event in one transaction) -> queue ack. Findings that shaped the design: outcomes persist BEFORE ack; RCPT is all-or-error; recipient rows existed but per-recipient status was never used; message status vocabulary and events had CHECK constraints. Research: RFC 3463 X.1.x/X.2.x/X.7.x read from the RFC text; RFC 5321 2.4 (local-part case) read; SES/SendGrid/Postmark suppression behavior as reference only. Design `docs/design-v0.30.md`; migration 000013 justified (durable policy state, tenant-safe, two query-driven indexes).

## 2026-09-21 | G4 | constructor | GATE PASS
`internal/suppression` (vocabulary, Normalize, QualifiesHardBounce), `database/suppressions.go` + migration 000013, `PersistDeliveryOutcome` atomic hard-bounce suppression, `worker/suppression.go` (`SuppressionGate`, enforcement before any transport), API handlers/routes/scopes/OpenAPI, acceptance check, metrics, `smtptest` RcptReplies/MailReply options.

## 2026-09-21 | G5 | verifier | GATE PASS
fmt, vet, builds (darwin, linux/amd64), `go test -count=1 ./...` and `-race` with real PostgreSQL/Redis: all pass, 0 skipped (1472 passes under race). Concurrency/stress suites repeated 8x under -race. Mutation check: disabling worker enforcement fails 8 of the new tests (they are not vacuous). Fuzzers: Normalize and the cursor decoder. Fake-MX connection counting proves zero SMTP for suppressed recipients (direct and relay), partial recipient filtering, suppression between retries, lookup/record failure deferral, ack-failure reclaim, cancellation, and the documented in-flight boundary. End to end with a real DB and the production adapter: hard bounce -> suppression -> later mail makes no connection -> unsuppress -> new mail attempted, old terminal messages never resurrect. Real `docker compose build` exit 0; a COPY of the real v0.29 dev database upgraded 12 -> 13 with all rows intact and the API/worker path exercised in throwaway containers (private Redis, non-resolvable `.invalid` domain, nothing sent); the real dev database and running container were untouched. Open risks RSK-020, RSK-023, RSK-026 unchanged (not resolved by this milestone). New risks RSK-027..030.

## 2026-09-21 | G6-G8 | release-manager | GATE PASS (local)
v0.30 complete pending commit. Not pushed. Next: v0.31 abuse controls (not started).

## 2026-09-22 | G0-G3 | conductor | v0.31 PLAN
Scope CR-014: outbound abuse controls (tenant/key request limits, recipient-volume limit, per-tenant queue cap, global backpressure, per-tenant/destination concurrency permits, fair dispatch, retry jitter). Baseline at `cd75bc6` green (26 packages, plain and race). Audit findings that shaped the design: `ListPendingOutbox` ordered globally by `available_at` (head-of-line starvation); Redis queue `Enqueue` blocks at capacity; idempotency claim precedes the FileStore write so a post-claim refusal must release it; suppression returns the deliverable count for charging; auth already fails closed on dependency errors (precedent). Research: RFC 6585 (429), RFC 9110 10.2.3 (Retry-After), GCRA/token-bucket references, RFC 5321 (100-recipient minimum), Redis Lua atomicity/Cluster hash tags. Design `docs/design-v0.31.md`; migration 000014 justified by EXPLAIN evidence.

## 2026-09-22 | G4 | constructor | GATE PASS
`internal/ratelimit` (GCRA `Allow`, lease permits, `Policy`), `internal/api/abuse.go` (middleware, `admitSend`, claim release), `database/abuse.go` + migration 000014 + fair `ListPendingOutbox`, `worker/permits.go`, `retry` jitter, `cmd/mailx/abuseconfig.go`, metrics, programmatic OpenAPI 429/503 with Retry-After, compose/.env docs.

## 2026-09-22 | G5 | verifier | GATE PASS
fmt, vet, builds (darwin, linux/amd64), `go test -count=1 ./...` and `-race` with real PostgreSQL and Redis: 27 packages, 1533 passes, 0 skipped. Mutation checks (8 deliberate breakages: claim release skipped, recipient cost 0, write fail-open, worker permits skipped, request limiter unwired, queue cap off, backpressure off, round robin broken) each fail tests. Concurrency: 40 parallel sends across 3 keys against a 10-token bucket accept exactly 10; two-process GCRA atomicity accepts exactly 100 of 400. Fuzzers: policy validation/Retry-After (10 s) and config loader (8 s) clean. Real `docker compose build` exit 0; a COPY of the dev database migrated 13 -> 14 (index swap verified) in throwaway containers on a private network (nothing sent; recipients `.invalid`), normal and low-memory profiles: 429 with Retry-After, queue cap 429 `tenant_queue_full` with the refused key reusable, invalid config exits 1 naming the variable, `off` warns loudly, limiter metrics bounded; the real dev containers and database were untouched. Defects DEF-020..022 (fixed). New risks RSK-031..034; RSK-029 remains open.

## 2026-09-22 | G6-G8 | release-manager | GATE PASS (local)
v0.31 complete pending commit. Not pushed. Next: v0.32 complaint/feedback loops (not started). OPERATIONAL NOTE: the running dev container still runs the v0.30 image; rebuilding it applies migration 000014 and turns the default limits ON (relevant to any script that sends fast).

## 2026-09-22 | G5 | verifier | PR #16 REVIEW FIXES
Greptile found 5 valid issues (DEF-023..027), all fixed with regression tests that each fail against the old code (mutation-checked). Full `go test -race ./...` with real PostgreSQL/Redis green. Architecture updated: queue cap counts retrying; permit holder is per claim; idempotency release is owner-guarded; every 503 carries Retry-After.
