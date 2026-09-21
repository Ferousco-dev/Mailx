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

- RSK-014 [medium, v0.25]: relay credentials are read from environment variables and stay in process memory; anyone with process-environment access (container inspect, /proc) can read them. Mitigation deferred: `_FILE`/secret-manager inputs and rotation.
- RSK-015 [low, v0.25]: wrong relay credentials keep queued mail retrying (bounded by backoff and 5 operations) until fixed; senders are not told until exhaustion. There is no relay health signal by design (liveness/readiness never call the relay); watch `mailx_smtp_auth_attempts_total{outcome="rejected"}`.
- RSK-016 [low, v0.25]: a single relay serves all tenants and domains; no per-tenant routing or authorization.

- RSK-006 [CLOSED in v0.26]: verified-From enforcement now exists.
- RSK-017 [medium, v0.26]: a domain with an active DKIM key refuses to send if its key cannot be decrypted (for example after a lost or wrong `MAILX_DKIM_MASTER_KEY`); recovery is to re-create keys, which requires the master key to be restored first or the domain re-keyed manually.
- RSK-018 [low, v0.26]: retired selectors must stay in DNS long enough for queued mail to verify (recommended >= 24 h; the default retry schedule spans about 7.5 h); MailX does not check.
- RSK-019 [low, v0.26]: DKIM proves domain control of the signing domain only; it does not affect SPF, DMARC alignment policy, or PTR/HELO deliverability (v0.27+).
- RSK-020 [medium, v0.27, STILL OPEN after v0.29: DNS/PTR matching a configured IP proves DNS consistency, not that the machine egresses from it; MailX will not call third-party IP services]: SPF guidance is only as good as the operator's `MAILX_SENDING_IPS`; a wrong (but public) address yields a truthful-looking but wrong record. MailX cannot detect NAT/egress mismatches. Mitigation: documented; verify egress with a real send in v0.29 testing.
- RSK-021 [CLOSED in v0.29, was low; public delivery no longer uses `mailx.local` and the HELO/Reporting-MTA identity is the configured hostname; still open in local mode by design]: HELO is `mailx.local` and bounces use the null reverse path, so receivers that check HELO for bounces get no usable SPF identity; deliverability of DSNs is affected (DSNs are also not transmitted yet, limitation 2). Address in v0.29.
- RSK-022 [CLOSED in the v0.28 correction, was low]: alignment uses the PSL while receivers following RFC 9989 use a DNS tree walk; results can differ for domains under public-suffix operators publishing `psd=` records. Not observed in practice for ordinary registrable domains.
- RSK-023 [medium, v0.28]: MailX cannot see receiver DMARC results or aggregate reports; "ready" is unverified against real receivers, and moving a domain to quarantine/reject is only safe if every legitimate sender of that domain aligns. MailX does not automate or check that.
- RSK-024 [medium, deployment]: AWS blocks outbound port 25 on Lightsail until a request is approved (up to 48 h, may be declined) and reverse DNS for a Lightsail static IP is set through AWS Support, not the console; AWS IP ranges have a mixed mail reputation. Direct delivery from the Lightsail IP is unproven; relay mode (`MAILX_RELAY_*`) is the fallback. Test with `nc -vz -w 5 gmail-smtp-in.l.google.com 25` from the server before relying on it.
- RSK-025 [low, deployment]: a 1 GB Lightsail host running MailX, PostgreSQL and Redis together has thin headroom (~265 MiB measured for the stack plus OS and Docker); PostgreSQL memory should be tuned and swap added, and backups (nightly `pg_dump` copied off the server, per DEC-070) must be arranged; the opt-in `compose.low-memory.yaml` profile (DEC-077) cuts PostgreSQL to ~51 MiB in a controlled test while keeping durability on because API keys, domains and encrypted DKIM keys live only in PostgreSQL.
- RSK-026 [low, v0.29]: the inbound receiver still greets `220 localhost MailX SMTP Server`; MailX has no public inbound role yet, so this is deferred with inbound work.
- RSK-027 [medium, v0.30]: a suppression created after the worker's check but during the same SMTP attempt cannot stop that attempt (documented consistency boundary; accepted-delivery immutability); mitigated by checking once per attempt as late as reasonable.
- RSK-028 [low, v0.30]: case variants of a local part share one suppression key by design; a hypothetical pair of mailboxes that differ only by case cannot be suppressed independently.
- RSK-029 [medium, v0.30]: RCPT is all-or-error, so one hard-bouncing recipient still fails the whole multi-recipient message; v0.30 suppresses that recipient for future messages but does not deliver the healthy recipients of the failed message.
- RSK-030 [low, v0.30]: hard-deleted suppressions leave no audit trail; a wrongly removed entry is only visible through logs/metrics counts. Also a PostgreSQL outage defers delivery (by design) instead of sending.
