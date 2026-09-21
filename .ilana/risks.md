# Risks

- RSK-001 [high]: structured logs may expose email content or credentials. Mitigation: allowlisted fields and marker-based privacy tests.
- RSK-002 [high]: unbounded metric labels may cause cardinality/resource exhaustion. Mitigation: fixed enumerated labels and exposition tests.
- RSK-003 [medium]: readiness semantics may trigger false-ready or restart loops. Mitigation: liveness independence, bounded PostgreSQL/Redis readiness checks, degradation tests.

## Recovered standing risks / technical debt (2026-09-21; full detail in `architecture.md` "Known limitations")

- RSK-004 [high]: SMTP acceptance followed by hard crash before PostgreSQL commit can cause a duplicate send after reclaim; no exactly-once SMTP is possible. Mitigation in place: durable-before-finalize + pre-SMTP durable check. Residual accepted.
- RSK-005 [medium]: DSNs are generated but never sent; senders of terminally failed API mail rely on events/webhooks/polling.
- RSK-006 [medium]: verified domain ownership is not enforced on `POST /v1/emails`; ownership currently has no send-time effect.
- RSK-007 [medium]: no TLS/STARTTLS on SMTP (v0.24 candidate); credentials/mail traverse plaintext SMTP.
- RSK-008 [medium]: readiness omits Redis and no metrics exist (v0.23 addresses; see SRS).
- RSK-009 [low]: webhook replay/ordering not provided; single master key without versioning.
- RSK-010 [low]: PostgreSQL integration tests use `MAILX_TEST_DATABASE_URL` if set, else try a local default DSN and SKIP if unreachable (`internal/database/testdb_test.go`), so an unset DSN silently loses coverage; Redis tests FAIL when Redis is unreachable. CI provisions both. Set the DSN explicitly for real validation.
- RSK-011 [low]: orphan FileStore directories after failed API transactions; no reaper.

- RSK-012 [medium, v0.24]: MX host is verified, but MX-to-recipient-domain binding relies on unauthenticated DNS (no DANE/MTA-STS). Mitigation deferred to a later milestone.
- RSK-013 [medium, v0.24]: fail-closed opportunistic policy means peers advertising STARTTLS with an untrusted/mismatched certificate receive no mail (retry, then exhaustion) until they fix it or an operator trusts their CA. Trade-off chosen over silent downgrade.
- RSK-007 [superseded by v0.24]: outbound SMTP now supports STARTTLS; inbound STARTTLS still absent (see architecture limitation 8).
