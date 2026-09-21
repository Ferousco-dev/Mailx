# MailX v0.26 Design: DKIM Signing, Key Lifecycle, Verified-From Enforcement

Scope: outbound DKIM signing, per-domain key lifecycle, and enforcement that a tenant may only send from domains it has verified. Out of scope: SPF, DMARC, PTR/HELO, MTA-STS, DANE, ARC, inbound DKIM verification, inbound SMTP AUTH, dashboards, reputation.

## Standards checked
RFC 6376 (fetched): required tags v/a/b/bh/d/h/s; From MUST be signed; hash = h= headers in order (repeated headers bottom-up) then the DKIM-Signature header with `b=` empty and no trailing CRLF; relaxed header canonicalization (lowercase name, unfold, collapse WSP, trim, no WSP around the colon) and relaxed body canonicalization (strip trailing WSP, collapse WSP, drop trailing empty lines, add final CRLF); TXT key record tags v/k/p/h/s/t. RFC 8301 (fetched): rsa-sha1 MUST NOT be used; signers MUST use RSA of at least 1024 bits and SHOULD use 2048; rsa-sha256 mandatory. Ed25519 (RFC 8463) was not adopted (see below); this was a judgement, not a fetched requirement.

## Decisions
- **Algorithm:** rsa-sha256, 2048-bit only. Keys under 2048 bits and non-RSA keys are refused at load; the schema CHECKs `algorithm = 'rsa-sha256'` and `key_bits >= 2048`. 2048 bits fits a TXT record (two character-strings); larger keys risk oversized DNS answers. Ed25519 deferred: uneven receiver support means it would need dual signing to be useful. All primitives come from crypto/rsa, crypto/sha256, crypto/rand, crypto/x509; MailX only assembles the DKIM protocol.
- **Canonicalization:** relaxed/relaxed (robust to header re-wrapping and whitespace normalization by intermediate MTAs; tests show unwrap/rewrap and trailing-space changes still verify while content changes do not). RFC 6376 3.4.5 examples are unit tests.
- **Signed headers (present ones only, in this order):** from, to, cc, subject, date, message-id, mime-version, content-type, content-transfer-encoding, reply-to. Not signed: Received, Return-Path, trace headers, and no `l=`, `x=` or `i=` tags. Bcc never exists in built messages (envelope only) and a regression test asserts it never appears in a signed message.
- **Tags:** v=1; a=rsa-sha256; c=relaxed/relaxed; d=<From domain>; s=<selector>; t=<unix>; h=...; bh=...; b=.
- **Selector:** `mx` + YYYYMMDD + 4 random hex digits (14 chars): a valid DNS label, dated so rotations are recognizable, no secret, no database ID, unique per domain (UNIQUE(domain_id, selector), regenerated on collision). DNS name: `<selector>._domainkey.<domain>`; value `v=DKIM1; k=rsa; p=<base64 SPKI>`; the API also returns the value split into <=255-byte character-strings.

## Verified-From enforcement
- `POST /v1/emails` authorizes the RFC 5322 From domain BEFORE the idempotency claim, FileStore write and every durable insert. Rule: the tenant must have a verified, non-deleted domain whose canonical name EXACTLY equals the parsed From domain. No suffix matching, no parent-covers-subdomain inference (v0.21 models roots and subdomains as independent resources). Parsing uses `net/mail` and the same `domain.Normalize` used for ownership (case-folded, one trailing dot stripped, IDN and non-ICANN names rejected), so authorization and ownership cannot disagree.
- Failure: 403 `from_domain_not_authorized` (also for another tenant's domain and for an unverified domain, so nothing about other tenants is revealed); malformed or non-domain From values are 422/403. `InsertMessage` re-checks the domain FOR SHARE in its own transaction, so a domain deleted after the pre-check cannot slip a message through.
- Envelope vs header: today MAIL FROM equals the From address (`built.From`), so both are the authorized identity. Bounce/return-path architecture is unchanged. The API is the only outbound message creator (the inbound SMTP listener only stores mail, nothing dequeues it), so this is the trustworthy boundary.

## Key storage and encryption
- Private keys: PKCS#8 DER encrypted with AES-256-GCM (`internal/secretbox`, extracted from the webhook secret box, which now wraps it), fresh random nonce per message, associated data `mailx-dkim-v1 | tenant | domain | selector` so a ciphertext cannot be moved to another row. Only ciphertext and nonce reach PostgreSQL; the DB is inspected in tests for plaintext markers.
- Master key: `MAILX_DKIM_MASTER_KEY` (base64 32 bytes), validated at startup, and it MUST differ from `MAILX_WEBHOOK_MASTER_KEY` (unrelated purposes never share key material; enforced). Errors name variables, never values. Go cannot guarantee memory zeroization: a decrypted key lives in process memory until garbage collected.
- Private keys are never returned by any endpoint and no resource type has a field that could carry them.

## Lifecycle (dkim_keys, migration 000012)
`pending` (generated, DNS not verified, does NOT sign) -> `active` (published and verified; the only signing key) -> `retired` (never signs again; private ciphertext destroyed at retirement). `uq_dkim_one_active` and `uq_dkim_one_pending` (partial unique indexes) plus a per-domain row lock make concurrent creation/activation resolve to exactly one winner (tests: 8-10 concurrent creates give 1 success and the rest 409 `dkim_key_pending`).
- Setup/rotation are the same operations: `POST /v1/domains/{id}/dkim` creates a pending key (409 if the domain is unverified or a pending key exists); publish the TXT; `POST .../dkim/verify` looks up the record via the existing TXT resolver, compares `p=` to the stored public key, then atomically retires the previous active key and activates the new one. The old key keeps signing until activation and its DNS record should stay published for at least 24 hours (the default retry schedule spans about 7.5 hours).
- DKIM readiness is explicit: domain ownership verified is not DKIM published. Only an ACTIVE key signs. A domain with no active key sends unsigned (DKIM not set up); a domain with an active key ALWAYS signs.
- Domain deletion deletes the domain's keys in the same transaction (ciphertext does not survive); a re-claimed name is a new domain row with no keys.
- Indexes: no new lookup indexes beyond the two partial unique indexes and `UNIQUE(domain_id, selector)`; the added `UNIQUE(tenant_id, id)` on domains backs the composite tenant-consistency foreign key. Hot query (domain by tenant+name joined to the active key): index scans on a `uq_domains_*` name index and `uq_dkim_one_active`, 6 buffers, ~0.02 ms on 6000 domains.

## Signing boundary and retries
Signing happens in the API at acceptance, after the MIME is final and before storage: the signed bytes are what FileStore persists, the queue references, and SMTP transmits (a worker-level test captures the DATA at a receiving MX for both direct and relay delivery and asserts byte equality with the stored message and independent DKIM verification). Retries resend the stored bytes; a message accepted under key A keeps its key-A signature even if key B becomes active before a retry (tested), so keep old selectors in DNS for at least 24 hours. Signing is transport-independent: the SMTP layers never see DKIM.

## No unsigned fallback
Missing-but-required, undecryptable, corrupt, mismatched or unusable keys, or a signing failure, make the send fail with 503 `dkim_signing_unavailable` before anything is accepted, stored or queued (tests: wrong master key, corrupt ciphertext, mismatched public key, unavailable store). Error text is a fixed category; no crypto detail is exposed.

## Observability
`mailx_dkim_signatures_total{algorithm,outcome}`: algorithm rsa-sha256; outcome signed, unsigned_no_key, key_unavailable, key_decrypt_failed, key_invalid, sign_failed, domain_mismatch; anything else becomes `other`. No domains, selectors, key data or error text. Liveness and readiness never perform DNS or DKIM checks.

## API (OpenAPI updated)
`GET /v1/domains/{id}/dkim`, `POST /v1/domains/{id}/dkim`, `POST /v1/domains/{id}/dkim/verify` under the existing domains:read/domains:write scopes; other tenants' domains are 404. `POST /v1/emails` documents the 403 and 503 behaviors.

## Deferred / limits
Ed25519 and dual signing; automatic key expiry and scheduled rotation; explicit key revocation/deletion endpoint (keys are removed with the domain or retired by rotation); DKIM DNS re-checks after activation; oversigning of headers; signing of messages from the inbound path (there is none); per-domain "require DKIM" flag (a domain requires DKIM exactly when it has an active key).
