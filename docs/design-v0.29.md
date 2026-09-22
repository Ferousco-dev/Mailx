# MailX v0.29 Design: public SMTP identity, PTR/rDNS, EHLO and DNS readiness

Scope: give MailX a truthful, operator-configured public SMTP identity and a way to check that DNS backs it up. It does not guarantee inbox placement, change SPF/DKIM/DMARC semantics, add persistence, or change delivery truth (final DATA 2xx = Accepted).

## Standards: requirements vs practice
**RFC requirements (verified against the RFC text):**
- RFC 5321 4.1.1.1: the EHLO/HELO argument carries the client's FQDN; where none is meaningful, the client SHOULD send an address literal; EHLO or HELO MUST be issued before a mail transaction. Syntax: `Domain / address-literal`.
- RFC 7208 2.3: verifiers are RECOMMENDED to check the HELO identity separately (only for a valid multi-label name); 2.4: with a null reverse-path the MAIL FROM identity is `postmaster@<HELO>`.
- RFC 1912 2.1: every address should have a PTR that matches its A record; multi-homed hosts need a PTR for EVERY address; PTR targets must be A records, not CNAMEs; host-name labels are ASCII letters, digits and hyphen.
- RFC 5322 3.6.4: every message SHOULD have a Message-ID that is globally unique; the right-hand side SHOULD be a domain of the host that generates it.

**Receiver/operator best practice (not RFC MUSTs):** Gmail-class receivers expect forward and reverse DNS for the sending IP and an EHLO name consistent with the PTR name; many score a missing PTR, an EHLO that does not match, or a generic ISP PTR negatively. MailX reports these as readiness and never enforces them at startup.

## Identity layers (never conflated)
| Layer | Owner | Controls |
|---|---|---|
| Tenant verified domain | tenant | From authorization, DKIM `d=`, SPF policy, DMARC alignment |
| Infrastructure hostname | the operator of the sending IPs | EHLO/HELO, PTR/rDNS, forward DNS, Message-ID domain, bounce Reporting-MTA |
The infrastructure hostname is configured (`MAILX_SMTP_HOSTNAME`), never derived from a tenant domain, and tenants never create PTR records. It is deliberately NOT aligned to any tenant's DMARC domain.

## Source audit: where `mailx.local` was used
`cmd/mailx/serve.go` client EHLO; `internal/api/email_handler.go` (two places: the message Message-ID and the stored metadata copy); `internal/worker/worker.go` DSN Reporting-MTA default. All now take one value (`smtpID.Name()`). Also seen and left alone: the inbound receiver greeting `220 localhost MailX SMTP Server` (inbound is out of scope; no Received headers are generated anywhere).

## Configuration and modes (`cmd/mailx/smtpidentity.go`)
- `MAILX_SMTP_HOSTNAME` set: validated strictly at startup (`smtpidentity.ValidateHostname`); invalid => startup fails naming the variable and a reason code, never the value.
- `MAILX_SENDING_IPS` set (public direct delivery declared): the hostname is REQUIRED, so public direct SMTP can never identify as `mailx.local`.
- Neither set: local/development identity `mailx.local` (docker compose, tests, fake MX). A relay without a hostname also runs on it, with a startup warning: the relay is the visible sender to recipients and PTR belongs to the relay operator.
- Startup validation is static. DNS is never consulted at startup, so a DNS outage cannot stop MailX; health endpoints never depend on it.

## Hostname validation
ASCII only, one trailing root dot removed, lower-cased, nothing repaired. Rejected: empty, over 253 bytes, whitespace/control, non-ASCII, `xn--` labels (same IDN policy as domain ownership), IP literals, URLs/paths, `host:port`, mailboxes, wildcards, `localhost`, reserved TLDs (`local`, `internal`, `lan`, `home`, `corp`, `private`, `test`, `example`, `invalid`, `arpa`, ...), single-label names, public suffixes alone (`co.uk`), invalid labels, and anything `domain.Normalize` (the ownership canonicalizer, ICANN suffix) refuses. Fuzzed.

## EHLO/HELO and Message-ID
- One identity for the whole session: EHLO before STARTTLS, EHLO after TLS, and HELO on fallback all carry the configured identity (byte-capture tests). The client rejects identities with whitespace/CR/LF/NUL. STARTTLS anti-injection, capability reset and TLS-before-AUTH are unchanged.
- Direct: the identity MailX shows recipient MXs. Relay: the same configured identity is used toward the relay (a submission relay accepts any FQDN); MailX does not check PTR for the relay's IPs.
- Message-ID: MailX ALWAYS generated it (the API accepts no user Message-ID or custom headers), as `<id@mailx.local>`. It is now `<id@MAILX_SMTP_HOSTNAME>` (RFC 5322 3.6.4: a domain of the generating host; the infrastructure host, not a tenant domain and not DKIM `d=`). Built in one helper before DKIM signing, so the signature covers it; stored bytes are never touched afterwards. Local mode keeps `mailx.local`. Message-ID does not determine spam placement.
- DSN Reporting-MTA uses the same name (DSNs are still not transmitted).

## Null reverse-path and HELO SPF (RFC 7208 2.3/2.4)
Receivers are recommended to check SPF for the HELO name itself, and for `MAIL FROM:<>` (bounces) it is the only identity (`postmaster@<HELO>`). With a configured hostname this becomes meaningful: publishing ONE SPF record at the hostname (`v=spf1 ip4:... -all`) makes bounce mail SPF-authenticated. It is advisory: not required by an RFC, so it never affects `ready` (warning `helo_spf_not_verified`); the diagnostic prints the exact record. It is not a tenant record and not generated through any tenant API. PTR itself is never SPF authorization, and SPF/DMARC semantics are unchanged (DMARC concerns the tenant From domain; the infrastructure hostname is not a DMARC identity).

## PTR / forward-confirmed reverse DNS (`internal/smtpidentity`)
Per configured IP: `LookupAddr` (Go's resolver builds `in-addr.arpa`/`ip6.arpa`), then the hostname is resolved once (`LookupNetIP`, A and AAAA).
- `ready`: a PTR name equals the hostname AND the hostname resolves back to this IP (membership, not equality: several addresses are normal). Extra PTR names warn (`multiple_ptr_records`: some receivers evaluate only one).
- `missing_ptr`, `ptr_mismatch` (PTR names some other host: EHLO and PTR unrelated is never ready), `forward_mismatch`, `malformed_ptr` (hostile/garbage PTR names are dropped, never reported), `oversized_dns_answer` (more than 16 PTR names or a forward answer beyond 64 addresses is fail-safe not-ready), `temporary_error` (SERVFAIL, timeout, cancellation, any resolver error: nothing concluded).
- Aggregate: any definite failure => `not_ready`; else any temporary => `unknown`; only all-IPs-ready => `ready`; a healthy IP never hides a broken one. Deterministic order. A report produced under a finished context is never `ready`.
- Bounds: 16 IPs, 10 s per check, 4 concurrent reverse lookups, one forward and one TXT lookup shared by all IPs, no regex or recursion. Not done (limitation): CNAME detection (RFC 1912 says PTR targets must not be aliases).

## Sending-IP model (RSK-020 stays open)
`MAILX_SENDING_IPS` is still operator-declared; nothing in v0.29 can prove a machine's true egress address without a third-party "what is my IP" service, which MailX deliberately never calls. DNS agreeing with the configured IP proves DNS consistency, not egress. One informational fact is added: whether each IP is assigned to a local interface (positive evidence on hosts with a public address; absent on NAT hosts such as cloud VMs, so it never changes readiness).

## Operator diagnostic, not an API
`mailx check-smtp-identity` (CLI; also `docker compose run --rm --no-deps mailx check-smtp-identity`) reads only `MAILX_SMTP_HOSTNAME` and `MAILX_SENDING_IPS`, prints per-IP state, PTR names, HELO SPF and advice, and exits non-zero unless `ready`. Reasons: PTR is the operator's, not a tenant's; an endpoint that resolved caller-supplied names/IPs would be an SSRF-style probe; health/readiness must not depend on public DNS. It accepts no arguments. No OpenAPI change, no metric (a short-lived CLI), no schema or index change: DNS is the source of truth.

## Limits / deferred
No egress proof (RSK-020); inbound receiver greeting still `localhost` (inbound out of scope); DSNs not transmitted; CNAME-target detection; no IPv6-specific DNSBL/reputation checks; inbox placement remains receiver-controlled.
