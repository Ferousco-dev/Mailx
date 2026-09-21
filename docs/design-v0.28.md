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

## Corrective pass: RFC 9989 DNS Tree Walk is the only authority (sections 4.10, 4.10.1, 4.10.2)
The first v0.28 commit used the ICANN Public Suffix List for the Organizational Domain (the RFC 7489 model) and a separate PSL-bounded walk for policy. That contradicted the RFC 9989 target, so it was replaced.

**One operation, one interpretation** (`treewalk.go`). A single walk from the From domain yields both the governing policy and the Organizational Domain, so they cannot disagree.
- 4.10 steps 1-2: query `_dmarc.<start>`; records whose first tag is not `v=DMARC1` are discarded; if MULTIPLE records remain they are ALL discarded and the walk CONTINUES; stop when a single record carries `psd=n` or `psd=y`.
- 4.10 steps 3-7: x = label count; x < 8: remove the left-most label; x >= 8: shorten to 7 labels; then remove one label at a time down to the TLD (`_dmarc.com` is queried). Hard cap: 8 queries per walked domain (the RFC's 12-label example is reproduced as a test).
- 4.10.2 Organizational Domain, from longest to shortest name that has a record: `psd=n` => that name; else a `psd=y` record other than the starting domain's own => the domain one label below it; else the name with the fewest labels that has a record; else the starting domain. A malformed record still counts as a retrieved record and its `psd` tag is read leniently, so it stops the walk and selects the organization exactly as the RFC says.
- 4.10.1 policy record: the Author Domain's record, else its Organizational Domain's, else its PSD's (`psd=y`); an intermediate record that is none of these does NOT govern. For the latter two the effective policy is `sp` (the domain exists) before `p`.
- Errors (left to receivers by the RFC): fail safe. Any DNS error other than "no such record" aborts the walk: no parent policy is substituted, no boundary is guessed, readiness is `unknown`. Relaxed alignment that needed such a boundary is `unknown`, never "not aligned".
- Multiple records: discarded per the RFC, but MailX reports `conflict` when they are at the domain itself (receivers silently fall back, so the owner must fix it) or when nothing else governs; a conflict elsewhere with a policy found is warning `conflicting_records_discarded`.

**Alignment** (`align.go`). Strict = identical canonical domains, no DNS. Identical domains align in either mode with no DNS (RFC 4.10.2). Relaxed = the two domains' Organizational Domains from the walk (memoized per verification; a second walk only when an identity differs from the From domain, which never happens in MailX where DKIM `d=`, the SPF/MAIL FROM domain and the From domain are equal). Without any published record there is no organization information and distinct names do not align (the RFC's default: the starting domain is its own Organizational Domain). No suffix matching, no label-count guessing, no PSL: a test forbids `publicsuffix`, `internal/domain`, `strings.HasSuffix` and `EffectiveTLDPlusOne` in the package's production code. `canonical()` is only a syntax gate (ASCII LDH, no IP literal, no IDN). The PSL remains in `internal/domain` for ownership validation only.

**Evidence the algorithms differ** (`TestOldPSLAndTreeWalkDisagree`, each case also asserts the old PSL answer differs from the RFC 9989 answer): psd=n makes a division its own organization; psd=y makes a domain a PSD; no published record; a PSO publishing psd=n at `co.uk`.

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
`mailx_dmarc_verifications_total{outcome,readiness}` (allowlisted; `other` otherwise); no domain, tenant, record, URI or error label or log field. Health endpoints never touch DMARC/DNS. Bounds: 5 s, 16 concurrent verifications (then 503 `dmarc_verification_busy`), 8 queries per walked domain. Resolver errors are never exposed. A DMARC verification also runs one SPF verification, which counts in `mailx_spf_verifications_total`.

## Limits / deferred
no aggregate/failure report intake, no external-destination authorization check; MAIL FROM/HELO/PTR identity work and bounce DMARC (v0.29); relay behavior is unmodelled beyond "unknown"; no policy automation or progression tooling; IDN domains unsupported (as everywhere).
