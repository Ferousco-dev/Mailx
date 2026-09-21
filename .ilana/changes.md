# Change Requests

- CR-001 [approved]: implement MailX v0.23 Logs / Observability without entering v0.24.
- CR-002 [approved, documentation-only, 2026-09-21]: reconstruct MailX v0.1-v0.22 knowledge in `.ilana/` from Git and add the permanent Ilana-update rule (`CLAUDE.md`). No production code, no v0.23/v0.24 work.
- CR-003 [approved, 2026-09-21]: implement v0.23 observability (SRS `docs/srs.md`, design `docs/design-v0.23.md`). Scope note: includes the DEF-004 worker resilience fix because the milestone's dependency-outage acceptance criteria cannot hold without it.
- CR-004 [approved, 2026-09-21]: address Greptile review of PR #14 (DEF-005..DEF-007); no scope beyond the three findings.
- CR-005 [approved, 2026-09-21]: implement v0.24 outbound SMTP STARTTLS (design `docs/design-v0.24.md`); v0.25 SMTP AUTH, DKIM/SPF/DMARC, inbound STARTTLS explicitly excluded.
- CR-006 [approved, 2026-09-21]: implement v0.25 outbound SMTP AUTH to a trusted relay (design `docs/design-v0.25.md`); inbound AUTH, OAuth, DKIM/SPF/DMARC excluded.
- CR-007 [approved, 2026-09-21]: implement v0.26 DKIM signing, key lifecycle and verified-From enforcement (design `docs/design-v0.26.md`); SPF, DMARC, PTR/HELO, inbound work and dashboards excluded.
- CR-008 [approved, 2026-09-21]: address Greptile review of PR #15 (DEF-011..DEF-013); no scope beyond the three findings.
- CR-009 [approved, 2026-09-21]: implement v0.27 SPF sending-authorization guidance (design `docs/design-v0.27.md`); DMARC, PTR/rDNS, HELO deliverability changes, real Internet delivery and any sending block excluded.
- CR-010 [approved, 2026-09-21]: implement v0.28 DMARC policy, alignment and DNS readiness (design `docs/design-v0.28.md`); PTR/rDNS/HELO, MTA-STS, DANE, ARC, BIMI, report intake and real Internet delivery excluded.
