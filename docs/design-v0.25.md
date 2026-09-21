# MailX v0.25 Design: SMTP AUTH / Trusted Relay Submission

Scope: OUTBOUND SMTP AUTH client to one explicitly configured trusted relay. Not in scope: inbound SMTP AUTH, MailX as a submission server, OAuth/XOAUTH2, provider integrations, credential vaults, DKIM/SPF/DMARC.

## Standards checked
- RFC 4954 (fetched): `AUTH` EHLO keyword lists mechanisms and MAY change after STARTTLS; exchange `AUTH mech [initial-response]`, 334 challenges, 235 success; 535 (5.7.8) invalid credentials is permanent, 454 (4.7.0) is temporary, 534 mechanism too weak, 530 authentication required, 504 unsupported mechanism; plaintext-password mechanisms MUST NOT be permitted without TLS (or equivalent protection); PLAIN over TLS is the mandatory-to-implement mechanism; do not use the initial response if the command would exceed the line limit.
- RFC 4616 (PLAIN: authzid NUL authcid NUL passwd, each field at most 255 octets), RFC 3207 (v0.24 STARTTLS).

## Mechanisms
Supported: PLAIN (preferred) and LOGIN (de-facto, still required by some relays), both only over verified TLS. Chosen because they are what submission relays overwhelmingly offer, PLAIN is the RFC 4954 mandatory mechanism, and both are safe only over TLS, which MailX already enforces.
Unsupported on purpose: CRAM-MD5 and DIGEST-MD5 (obsolete, weak, need password-equivalent storage), XOAUTH2/OAUTHBEARER (provider-specific token flows, later), SCRAM (rare on submission servers; add when a target needs it). If a relay advertises only these, AUTH fails with `no_mechanism`.

## State machine (`internal/smtp/client_auth.go`, `client.go run`)
```
220 -> EHLO -> STARTTLS -> handshake -> EHLO again (post-TLS capabilities)
    -> AUTH advertised in THAT reply? -> choose PLAIN, else LOGIN -> AUTH exchange
    -> 235 -> MAIL FROM -> RCPT TO -> DATA
```
Invariants (all tested):
1. Credentials never travel in plaintext: a session that carries credentials behaves as TLS-required regardless of `MAILX_SMTP_TLS_POLICY`. No STARTTLS advertised, STARTTLS refused, handshake failure or certificate failure all end the attempt before any AUTH byte. `authenticate` additionally refuses if TLS is not established (defense in depth).
2. The mechanism is chosen only from the post-TLS EHLO. A pre-TLS AUTH advertisement is never used (test: pre-TLS PLAIN, post-TLS only CRAM-MD5 => no AUTH sent).
3. MAIL FROM is unreachable until AUTH returns 235 (test with a server that answers MAIL with 530 until authenticated).
4. AUTH success is not delivery: `Accepted=true` still requires the final DATA 2xx; QUIT or teardown failure afterwards leaves it true (tested through the relay path at client and worker level).

## Direct vs relay (`internal/delivery`)
`Config.Relay` (nil by default) routes EVERY delivery through one configured relay: no MX lookup, one destination, the only credentials in the system. Direct mode resolves MX hosts and carries none; the credentials are reachable only through `Config.Relay`, so a routing bug cannot hand them to a DNS-discovered MX. Tests: direct engine attempts (including MX fallback) all carry `Auth == nil`; a real direct MX that offers AUTH never receives an AUTH command; relay mode never calls the resolver.
No silent downgrade: if the relay fails for any reason (dial, greeting, TLS, AUTH), the delivery fails and is retried later; it never becomes a direct attempt (tests at engine and worker level; the would-be direct MX sees zero connections).

## Failure classification and retry
New stage `auth`. SMTP semantics are kept on the error (454 temporary; 535/534 permanent; connection loss, timeout, cancellation temporary; malformed replies and no-mechanism are non-temporary). The delivery engine maps EVERY auth-stage failure to a temporary attempt (`stopTemporary`, never another MX): authentication failures describe MailX's relay configuration, not the message or recipient, so bouncing the sender permanently would punish them for an operator mistake. The existing backoff (30 minutes doubling to 4 hours, at most 5 operations) bounds the retry rate; a test with 20 jobs and wrong credentials shows exactly one AUTH attempt per job and retries scheduled at least 20 minutes out. There is no separate AUTH retry system.

## Secrets
- `smtp.Credentials` prints `smtp.Credentials{redacted}` for every fmt verb and slog.
- Remote AUTH reply text is discarded (only code and enhanced status are kept) because servers sometimes echo credentials; `DeliveryError`/`authFailure` text is a fixed category.
- Credentials are copied per Send and per engine, validated (non-empty, at most 255 bytes, no CR/LF/NUL) with value-free errors; startup errors name the variable, never the value.
- Base64 is treated as plaintext: tests search logs, metrics, operational errors and persisted attempt results for the raw username/password and for base64(username), base64(password) and base64(PLAIN payload) across success, wrong password, temporary error, malformed reply, disconnect and stall.
- Not done: Go strings cannot be zeroed, so secrets stay in process memory (and in the environment) for the process lifetime.

## Configuration
`MAILX_RELAY_HOST` (enables relay mode), `MAILX_RELAY_PORT` (default 587), `MAILX_RELAY_USERNAME`, `MAILX_RELAY_PASSWORD`. Unset host means direct delivery; any other RELAY variable without a host, a username without a password (or the reverse), a bad port or host, or control characters is a startup error. Port 465 (implicit TLS) is not supported. No insecure or plaintext option exists. Compose and `.env.example` carry no credentials (placeholders are commented out).

## Observability
`mailx_smtp_auth_attempts_total{mechanism,outcome}`: mechanism in {plain, login, none}; outcome in {success, rejected, temporary, no_tls, not_advertised, no_mechanism, protocol_error, connection_lost, timeout, canceled}; anything else collapses to `other`. The worker `delivery_outcome` log gains `transport` (direct|relay), `auth_mechanism`, `auth_outcome`. No user names, hosts, AUTH data or server text. Liveness and readiness are unchanged and never contact the relay.

## Timeouts
Each AUTH round trip is bounded by the client's read timeout (5 minutes default, a per-reply RFC 5321 style bound) and the caller's context; cancellation sets an immediate deadline on the socket. LOGIN has three rounds, each bounded. Tests cover stall before the reply, stall mid-LOGIN, disconnect and cancellation, with no goroutine leak.

## Deferred / limits
Inbound SMTP AUTH and submission server; OAuth/XOAUTH2; SCRAM; implicit-TLS (465); credential rotation without restart and secret-file (`_FILE`) inputs; multiple relays or per-tenant/per-domain routing; persisting AUTH facts per attempt (logs and metrics only; no migration).
