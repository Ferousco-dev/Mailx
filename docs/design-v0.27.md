# MailX v0.27 Design: SPF sending authorization guidance

Scope: help a tenant publish a correct SPF record for a domain MailX sends from, and check it. Not a receiving-side SPF engine, not DMARC (v0.28), not PTR/HELO deliverability (v0.29), not an authorization mechanism.

## Standards checked (RFC 7208)
- SPF authenticates the connecting IP against the **MAIL FROM** domain (RFC 7208 2.4); for the null reverse path (bounces) the **HELO** identity is used (2.3, 2.4). A record is a TXT starting `v=spf1` followed by a space or end (4.5); more than one SPF record on a name is a `permerror` (4.5).
- Terms are evaluated left to right; `all` ends evaluation and terms after it are ignored (5.1). Qualifiers `+ - ~ ?`, default `+` (4.6.2). Unknown modifiers are ignored, unknown mechanisms and malformed CIDR are a `permerror` (4.6, 5, 6). `redirect` applies only when no mechanism matched (6.1).
- Evaluation may cause at most 10 DNS-querying terms (`include a mx ptr exists redirect`) (4.6.4).

## SPF identity in MailX (verified against the code)
- **MAIL FROM** is the API `From` address (`email_handler.go`: `MailFrom: built.From`), i.e. the tenant's verified domain. So the identity SPF checks is the sender's own domain and the record goes on that domain. Bounces use the null reverse path (`internal/bounce`), for which receivers check HELO.
- **HELO/EHLO** identity is the constant `mailx.local` (`cmd/mailx/serve.go`). It is not a public name, so it cannot carry an SPF record. Fixing that is deliverability work for v0.29 and is deliberately not changed here; this milestone documents it.
- Header From is unchanged. SPF says nothing about it (alignment is DMARC, v0.28).

## Architecture (`internal/spf`)
- `record.go`: bounded parser (no regex, no recursion) and pure predicates. `sending.go`: `Config` (mode, public IPs, relay include) and public-address validation. `service.go`: `Analyze` (pure) and `Service` (tenant-scoped, one TXT lookup, bounded concurrency).
- API (`internal/api/spf_handler.go`): `GET /v1/domains/{id}/spf` (domains:read; guidance only, **no DNS**), `POST /v1/domains/{id}/spf/verify` (domains:write; one bounded lookup; requires a verified domain so it cannot probe arbitrary DNS; changes nothing). Cross-tenant and unknown ids are indistinguishable 404.
- Import boundaries are tested: `internal/spf` cannot import smtp/delivery/worker/dkim/queue/secretbox, and the transport/send/DKIM packages and the send handler cannot reference SPF.

## Sending-IP model and modes
- **Direct mode** (no `MAILX_RELAY_HOST`): MailX connects from addresses it cannot reliably discover (NAT, gateways). The operator declares them in `MAILX_SENDING_IPS` (public IPv4/IPv6, max 16, single addresses). Loopback, private, link-local, CGNAT, documentation, multicast, reserved, ULA and other special-purpose ranges are rejected at startup, and the service constructor re-checks, so non-public addresses can never be advertised. Unset means "unknown": the API says `sending_infrastructure_unknown` and generates no record.
- **Relay mode** (`MAILX_RELAY_HOST`): MailX does not send from its own addresses, and it cannot know the relay's SPF requirements or whether it rewrites the return-path. MailX invents nothing. The operator may set `MAILX_SPF_RELAY_INCLUDE` (the include domain the provider documents); otherwise the action is `follow_relay_provider`. `MAILX_SENDING_IPS` in relay mode, or the include in direct mode, is a startup error (it would state something false).
- SPF never changes routing, credentials, STARTTLS or AUTH (structurally enforced, see boundaries).

## DNS record model
- Missing: `create` with `v=spf1 <ip4:/ip6: mechanisms | include:relay> ~all`. `~all` is the conservative default; the tenant picks the final qualifier.
- Existing single record: verified when it literally authorizes MailX; otherwise `update_existing` returns **the same record extended** (mechanisms inserted at the front so they match before any restrictive term). MailX never suggests publishing a second SPF record. Multiple records: `conflict`, action `merge_records`, no generated value.
- Merged output over the 2048-byte bound is refused (`merged_record_too_long`), never truncated.

## Verification semantics (distinct states, not one boolean)
`verified` (record literally authorizes MailX's infrastructure), `not_configured`, `mismatch` (includes explicitly non-passing listings such as `~ip4:` and `+all` without a literal match), `conflict`, `invalid` (bounded reason code), `temporary_error` (timeout, SERVFAIL, any resolver failure, never reported as misconfiguration), `sending_infrastructure_unknown`, `unchecked` (GET). v0.28 DMARC can consume these individually.

## DNS lookup-limit decision
MailX validates only the **literal subset**: ip4/ip6 (direct) or a named include (relay). `include a mx exists ptr redirect` are parsed but never followed: walking a tenant-controlled DNS tree is unbounded work and full RFC evaluation is a receiving-side engine. A record that authorizes MailX only through those terms reports `mismatch` with `unevaluated_mechanisms: true` rather than claiming it works. `dns_lookup_limit_risk` warns when the record already uses 8 or more such terms (counting the include MailX would add). Exactly one DNS query per verification.

## Bounds and security
5 s timeout; 16 concurrent lookups (wait honours context, then 503 `spf_verification_busy`); at most 64 TXT records; SPF record 2048 bytes; 255 bytes/term; 64 terms; control/non-ASCII bytes rejected. TXT text is untrusted: never logged, never a metric label, never in `reason`/`warnings`; the API returns the single published record only as a JSON string field. Resolver errors are never exposed. A fuzz target covers the parser.

## Sending policy (deliberate decision, DEC-recorded)
MailX **does not block sending** when SPF is missing, wrong or unverifiable. Authorization = verified domain ownership (unchanged); signing = active DKIM key (unchanged); SPF = readiness/deliverability information. The send path never consults SPF (tested with a resolver that fails everything). A later production policy may require all three, as a separate explicit decision. Local development (localhost, Compose, fake SMTP servers) needs no public SPF; production DNS validation is not weakened for tests.

## Persistence, indexes, observability
No schema change: DNS is the source of truth and persisting it would create stale "truth". No index changes, so no EXPLAIN. Metric `mailx_spf_verifications_total{mode,outcome}`, both allowlisted (`other` otherwise); no domain, tenant, IP, record or error label. Liveness/readiness do not touch SPF or public DNS. Delivery truth (final DATA 2xx = Accepted) and queue semantics are untouched.

## Limits / deferred
No SPF header generation and no Authentication-Results (MailX is not a verifier). No recursive evaluation. No IPv6 privacy-address detection or egress auto-discovery. HELO identity is `mailx.local` (v0.29). DMARC alignment, PTR/rDNS, MTA-STS/DANE deferred. Relay return-path rewriting is provider-specific and unmodelled.
