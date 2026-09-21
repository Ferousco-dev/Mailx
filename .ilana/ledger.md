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
