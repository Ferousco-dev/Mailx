# MailX v0.24 Design: Outbound SMTP STARTTLS

Scope: transport security for MailX's OUTBOUND SMTP client only. It is not end-to-end encryption and not authentication of the recipient's mail system beyond what is stated below. Inbound STARTTLS, SMTP AUTH (v0.25), DKIM/SPF/DMARC, DANE and MTA-STS are out of scope.

## Standards checked
- RFC 3207 (fetched and read): keyword `STARTTLS` with no parameters; `220` means proceed; `454` TLS temporarily unavailable; `501` syntax; after a successful negotiation the protocol resets to its initial state and both sides discard knowledge not learned from the TLS negotiation; the client SHOULD send EHLO first after TLS; a publicly referenced SMTP server MUST NOT require STARTTLS.
- RFC 5321 3.2 (EHLO, HELO fallback on 5xx), RFC 8996 (TLS 1.0/1.1 deprecated).
- Go `crypto/tls`: `Config.MinVersion` defaults to TLS 1.2; `HandshakeContext` interrupts on context cancellation; `CertificateVerificationError` and `x509.*Error` types for verification failures.

## State transition (`internal/smtp/client_tls.go`, `client.go`)
```
TCP connect -> 220 -> EHLO (HELO fallback on 5xx) -> parse capabilities
  -> policy decision:
       STARTTLS not advertised: opportunistic -> continue plaintext (outcome not_offered)
                                required      -> fail before MAIL FROM (required_unavailable)
       advertised: STARTTLS -> must be 220 (else fail, no plaintext continuation)
  -> reject if the peer sent bytes after its 220 (STARTTLS command injection)
  -> DISCARD capabilities (s.caps = nil)
  -> tls.Client(raw conn).HandshakeContext (bounded by HandshakeTimeout and ctx)
  -> swap I/O to the TLS conn, new reader
  -> EHLO again (no HELO fallback), parse NEW capabilities
  -> MAIL FROM / RCPT TO / DATA
```
`raw` (the TCP conn) never changes so the context watcher can set deadlines without racing the upgrade; `conn` is what protocol I/O uses.

## Policy
- `TLSOpportunistic` (default) and `TLSRequired` (`MAILX_SMTP_TLS_POLICY`). Zero value is opportunistic.
- Default rationale: RFC 3207 forbids public servers from requiring STARTTLS, so plaintext must remain possible when STARTTLS is NOT offered.
- No silent downgrade: once STARTTLS is attempted, a refusal, malformed reply, handshake failure, verification failure or post-TLS EHLO failure is a delivery-attempt failure. Nothing is retried in plaintext on that connection or by reconnecting. Note this is stricter than some MTAs, which retry plaintext after a failed handshake.
- A server that advertises STARTTLS and then refuses it (even 5xx) is treated as a failure, not a reason to continue in plaintext: it is either misconfigured or an active downgrade attempt.

## Verification (what is and is not verified)
- Verified: the certificate chains to the system trust roots (plus any roots in `MAILX_SMTP_TLS_CA_FILE`), is within its validity period, is valid for server authentication, and matches the MX host name MailX connected to (`ServerName` = host part of the MX address; an IP literal is matched against IP SANs).
- Not verified: that the MX host is the correct MX for the recipient domain. MX records come from unauthenticated DNS. DANE (RFC 7672) and MTA-STS (RFC 8461) are deferred, so an attacker who can forge DNS can point MailX at a host with a valid certificate for its own name. v0.24 protects against passive observers and against attackers who cannot forge DNS, not against DNS forgery.
- There is no `InsecureSkipVerify` anywhere in production code and no setting that disables verification. Consequence of the default: a peer that advertises STARTTLS with a self-signed or mismatched certificate will not receive mail from MailX until fixed (or trusted through `MAILX_SMTP_TLS_CA_FILE`).
- TLS version: minimum TLS 1.2 (stated explicitly, equal to Go's default), maximum per Go. Cipher suites are Go's defaults.

## Timeouts and cancellation
Dial: `DialTimeout`. Each SMTP reply read: `ReadTimeout` (covers greeting, EHLO, STARTTLS reply, post-TLS EHLO). Handshake: `TLS.HandshakeTimeout` (default 30 s) and the caller context. Cancellation sets an immediate deadline on the raw conn (unblocking any TLS read/write) and `HandshakeContext` returns on cancel. Session close bounds the close_notify write to 1 s. Tests assert no goroutine leak after stalls and cancellation.

## Error classification (integrates with the existing model; no new retry system)
New stages: `starttls`, `tls_handshake`, `ehlo_tls`. Every TLS-stage `DeliveryError` is `Temporary` (a statement about this connection, not the recipient). The delivery engine treats these stages like pre-session failures: they happen before MAIL FROM, so it tries the next MX with the same policy and verification; if all fail, the result is `KindTransferTemporary` and the existing retry engine schedules a later attempt. Bounded outcome categories: established, not_offered, required_unavailable, rejected, handshake_timeout, verify_failed, handshake_failed, connection_lost, ehlo_failed, canceled, protocol_error. The error text under `DeliveryError` is a `TLSFailure` whose `Error()` is only the category; raw TLS/x509 text (which can contain peer-controlled names) is reachable only through `Unwrap`.

## Delivery truth
Unchanged. Final DATA 2xx sets `Accepted=true`; a QUIT failure or TLS teardown afterwards is recorded on `QuitError`, and the worker persists the outcome before acking. Tested at client, and worker pipeline level.

## Observability (v0.23 architecture)
- Metric `mailx_smtp_tls_sessions_total{policy,outcome,version}`: policy in {opportunistic, required}; outcome in the 11 categories above; version in {1.2, 1.3, other, none}. Values are allowlisted; anything else becomes `other`.
- The worker's `delivery_outcome` log gains `tls_policy`, `tls_outcome`, `tls_version` (last MX tried), alongside existing job/message IDs. No host names, certificates, addresses or raw errors are logged.
- Liveness and readiness are unchanged and never depend on remote SMTP/TLS.

## Configuration
`MAILX_SMTP_TLS_POLICY` (opportunistic|required, validated at startup) and `MAILX_SMTP_TLS_CA_FILE` (optional extra PEM roots added to the system pool; the path is never echoed). No migrations. The Docker runtime image now installs `ca-certificates` (alpine ships none, which would have made every verification, including HTTPS webhooks, fail in the container).

## Inbound
The inbound listener does not advertise or accept STARTTLS (test asserts this). Deferred: it needs certificate provisioning and reload, which is a separate concern.

## Deferred / known limits
Inbound STARTTLS; DANE and MTA-STS; per-domain policy overrides; unverified opportunistic encryption mode (deliberately not offered); persisting TLS facts per attempt (in-memory and logs/metrics only, no schema change).
