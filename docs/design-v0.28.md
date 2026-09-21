# MailX v0.28 Design: DMARC policy, alignment and DNS readiness

Scope: sender-side DMARC readiness. MailX inspects a domain's published DMARC policy, models how its DKIM and SPF identities align with the From domain, and recommends a record. It is NOT a receiving-side DMARC engine: it never evaluates messages, generates Authentication-Results/DMARC-Signature, blocks or authorizes sending, or claims a receiver's DMARC result.

## Standards checked
- **RFC 9989 (DMARC) is the current standard**; with RFC 9990 (aggregate reporting) and RFC 9991 (failure reporting) it obsoletes RFC 7489 (verified on rfc-editor.org: 7489 is Informational, obsoleted by 9989/9990/9991; published May 2026). Used as the source of truth; compatibility notes below.
- Changes relative to 7489 that MailX follows: `pct`, `rf`, `ri` removed (ignored with warning `deprecated_tag`); `np`, `psd`, `t` added; the organizational domain is found by a DNS Tree Walk (max 8 queries) instead of the Public Suffix List; multiple DMARC records at one name are all discarded; `p` is RECOMMENDED, and a record without `p` but with a valid `rua` acts as `p=none`; unknown tags MUST be ignored; the first tag MUST be `v=DMARC1`; only `mailto:` report URIs must be supported and other schemes are ignored.
- RFC 7208 (SPF authenticated identifier for DMARC is the MAIL FROM domain, never HELO; null reverse-path uses postmaster@HELO), RFC 6376/8301 (DKIM `d=`).

## Identity model (verified in code)
- RFC 5322 From = the API `from` (must be a verified domain of the tenant, exact match; v0.26).
- MAIL FROM = the API From address (`email_handler.go`; test `TestMailFromEqualsFromDomain`; the stored path keeps SMTP angle brackets and the sender's letter case, compared case-insensitively). So the SPF identity domain = From domain.
- DKIM `d=` = the From domain (v0.26 signs with the message From domain and refuses a mismatch).
- HELO/EHLO = `mailx.local`: irrelevant to DMARC SPF alignment (RFC 9989 uses only MAIL FROM), but it is why bounce mail has no usable SPF identity (v0.29).
- Consequence: in direct mode both paths align exactly (strict and relaxed).

## Alignment (`internal/dmarc/align.go`)
Pure functions independent of HTTP. `Aligned(from, id, mode)`: strict = identical canonical domains; relaxed = same organizational domain. Organizational domain = registrable domain from the Public Suffix List (ICANN section) through `domain.Normalize`, the same canonicalization as ownership. Never a suffix test: `attackerexample.com`, `example.com.attacker.com`, `com`, `co.uk`, IDN, IP literals and private-suffix names (`x.blogspot.com`) never align. Decision: PSL, not the RFC 9989 tree walk, for alignment (offline, deterministic, no attacker-influenced DNS in the decision); the two differ only where a public-suffix operator publishes `psd=` records. Recorded as a limitation.

## Policy discovery
`_dmarc.<domain>` TXT (never the SPF root). If that name has no DMARC record, parent names up to the organizational domain are queried (at most 8 queries in total). The first name with DMARC-looking records governs: one valid -> parse; several -> `conflict` (receivers discard all of them, so the owner must fix it; MailX does not fall through to the parent); malformed -> `invalid`; temporary DNS failure at any level -> `temporary_error` (a weaker parent policy is never substituted). A record inherited from the organizational domain reports source `organizational_domain` and `sp` applies as the effective policy.

## Parser (`record.go`)
Bounded (2048-byte record, 32 tags, 1024-byte values, 16 report URIs, 64 TXT records), no regex, no recursion, tolerant of `;` at the end and whitespace, case-insensitive names/values. Known tags with invalid values or duplicates make the record `invalid` (the owner's intent would be ambiguous even where receivers would default). Unknown tags are ignored. `rua`/`ruf`: only usable `mailto:` addresses counted, others warn; `external_report_destination` warns when a rua host is outside the domain's organization (report authorization is not verified: MailX has no report intake). Fuzzed (`FuzzParse`).

## Recommended policy: `v=DMARC1; p=none`
Monitoring first: a new or misconfigured sender that starts at `quarantine`/`reject` can lose legitimate mail, and MailX cannot see receiver results or reports to prove alignment holds for all of a domain's senders. Progression (none -> quarantine -> reject) is the domain owner's decision after confirming every sender authenticates and aligns; MailX never rewrites an existing policy (a published `p=reject` is preserved, action `none`) and never proposes a second record. No `rua` is generated: MailX has no aggregate-report intake and will not point reports at a mailbox that does not exist. `ruf` is never generated (privacy-sensitive, weakly supported; RFC 9991 keeps it optional). No dashboard exists.

## Readiness vs receiver result
`dns.status`: unchecked | not_configured | monitoring | enforcing | conflict | invalid | temporary_error. Per path (`dkim`, `spf`): ready | not_configured | not_aligned | unknown. `readiness`: ready (valid record and at least one path aligned AND configured) | dns_action_required | authentication_incomplete | unknown | unchecked. `receiver_result` is always `not_observed`: "ready" is a precondition for a receiver to pass DMARC, never proof that it did; `p=reject` does not mean inbox delivery. DKIM path = an ACTIVE key (from DKIM state, as a fact); SPF path = direct mode and a verified SPF record (from one bounded SPF verification, as a fact). In relay mode the relay may rewrite the return-path and MailX cannot know, so the SPF path is `unknown` (`relay_return_path_unknown`, identity empty), never faked; `relay_may_alter_signed_content` warns that a relay may break MailX's DKIM signature.

## Null reverse-path
DSNs are generated but not transmitted (known limitation 2), so there is no bounce mail to align today. If they are sent later with `MAIL FROM:<>`, SPF uses `postmaster@<HELO>` = `mailx.local`, which cannot align with any From domain; only a DKIM signature aligned with the DSN's From domain could provide DMARC. Deferred to the v0.29 public-identity work; nothing implemented here.

## API, sending policy, persistence
`GET /v1/domains/{id}/dmarc` (domains:read; guidance, no DNS) and `POST /v1/domains/{id}/dmarc/verify` (domains:write; verified owned domain required, else 409; cross-tenant/unknown 404; bounded DNS; stores nothing). Sending is never blocked by missing/invalid DMARC (authorization = verified ownership, signing = active DKIM, SPF/DMARC = readiness); DMARC cannot grant From authorization (tested). No schema or index change: DNS is the source of truth. No new configuration variables. DKIM and SPF reach `dmarc` through interfaces implemented by adapters in `cmd/mailx` (`dmarc` imports neither package; import boundaries are tested in both directions, and the send handler must not mention DMARC).

## Observability and security
`mailx_dmarc_verifications_total{outcome,readiness}` (allowlisted; `other` otherwise); no domain, tenant, record, URI or error label or log field. Health endpoints never touch DMARC/DNS. Bounds: 5 s, 16 concurrent verifications (then 503 `dmarc_verification_busy`), 8 queries. Resolver errors are never exposed. A DMARC verification also runs one SPF verification, which counts in `mailx_spf_verifications_total`.

## Limits / deferred
PSL vs tree-walk differences (psd); no aggregate/failure report intake, no external-destination authorization check; MAIL FROM/HELO/PTR identity work and bounce DMARC (v0.29); relay behavior is unmodelled beyond "unknown"; no policy automation or progression tooling; IDN domains unsupported (as everywhere).
