package api

import "net/http"

// openAPISpec is hand-written (contract-first), not generated from Go
// types/routes. Rationale (see the v0.18 report's "OpenAPI architecture"
// section): the public JSON schema is a deliberate product surface with
// its own naming/shape decisions (e.g. "html"/"text", not Go field
// names), and MailX still has a small route surface — a generator would
// buy safety against drift at the cost of a dependency this milestone
// does not otherwise need. openapi_test.go keeps it from silently
// diverging from the real handlers by asserting the documented routes
// and schemas match runtime behavior.
const openAPISpec = `{
  "openapi": "3.0.3",
  "info": {
    "title": "MailX API",
    "version": "v1",
    "description": "MailX's developer-facing email API. Every /v1 route requires a MailX API key (see securitySchemes.ApiKeyAuth) - management of the keys themselves (create/rotate/revoke) is a CLI/admin operation in v0.19, not a public REST endpoint; see the project's v0.19 report for why."
  },
  "servers": [{"url": "/v1"}],
  "security": [{"ApiKeyAuth": []}],
  "paths": {
    "/emails": {
      "post": {
        "summary": "Send an email",
        "description": "Requires the emails:send scope. Durably accepts an email for asynchronous processing. The From domain must be a domain your account has VERIFIED (exact match; a verified root does not authorize its subdomains): otherwise 403 from_domain_not_authorized, before anything is stored. If the From domain has an active DKIM key the message is signed with it at acceptance; if that key cannot be used the request fails with 503 dkim_signing_unavailable and nothing is accepted (never sent unsigned). All to/cc/bcc recipients must share one delivery domain; mixed-domain requests receive 422 before acceptance. Suppression: every recipient must be a plain ASCII address (422 invalid_recipient) and if EVERY recipient is on the account's suppression list nothing is stored (422 all_recipients_suppressed). If only some are suppressed the email is accepted and those recipients are skipped at delivery time with no SMTP attempt (their recipient state is 'suppressed', not failed or bounced); if all recipients become suppressed after acceptance the email ends as terminal status 'suppressed'. Suppression is re-checked immediately before every delivery attempt, including retries; a suppression created while an SMTP conversation is already in progress does not revoke that attempt, and an accepted delivery is never rewritten. If MailX cannot read suppression state it does not send: the attempt is deferred. A recipient that permanently rejects with 5.1.1 or 5.1.6 at RCPT TO is suppressed automatically (reason hard_bounce); temporary failures and sender, policy or authentication failures never suppress. 202 means MailX has validated and durably recorded the email and has durable responsibility for eventually attempting delivery - it does NOT mean the email has been delivered, that the recipient's server accepted it, or that Redis currently has the job. Retrying safely: supply the same Idempotency-Key on retry to get the original result back instead of creating a second email; this prevents duplicate MailX email SUBMISSIONS from a repeated HTTP request - it does not and cannot guarantee exactly-once SMTP delivery to the recipient's server.",
        "parameters": [
          {
            "name": "Idempotency-Key", "in": "header", "required": false,
            "schema": {"type": "string", "maxLength": 255},
            "description": "Optional, client-generated, opaque (a UUID is a good choice). Replaying the SAME key with the SAME request body returns the original email (marked with an Idempotency-Replayed: true response header) instead of creating a new one. Replaying the same key with a DIFFERENT body returns 409. MailX guarantees this replay behavior for at least 24 hours from the first use of a key; after that window a key may be reused for a new, unrelated submission. Scoped to your tenant, not to the specific API key used - rotating keys does not break in-flight retries."
          }
        ],
        "requestBody": {
          "required": true,
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/SendEmailRequest"}}}
        },
        "responses": {
          "202": {
            "description": "Accepted",
            "headers": {"Idempotency-Replayed": {"description": "Present and \"true\" only when this response replays an earlier result for the same Idempotency-Key.", "schema": {"type": "string"}}},
            "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Email"}}}
          },
          "400": {"$ref": "#/components/responses/Error"},
          "401": {"$ref": "#/components/responses/Error"},
          "403": {"$ref": "#/components/responses/Error"},
          "409": {"$ref": "#/components/responses/Error"},
          "413": {"$ref": "#/components/responses/Error"},
          "415": {"$ref": "#/components/responses/Error"},
          "422": {"$ref": "#/components/responses/Error"},
          "500": {"$ref": "#/components/responses/Error"},
          "503": {"$ref": "#/components/responses/Error"}
        }
      },
      "get": {
        "summary": "List emails",
        "description": "Requires the emails:read scope.",
        "parameters": [
          {"name": "limit", "in": "query", "schema": {"type": "integer", "minimum": 1, "maximum": 100, "default": 20}},
          {"name": "cursor", "in": "query", "schema": {"type": "string"}, "description": "Opaque; from a previous response's next_cursor. Never construct one."},
          {"name": "status", "in": "query", "schema": {"type": "string", "enum": ["queued", "processing", "retrying", "delivered", "failed", "bounced", "suppressed"]}}
        ],
        "responses": {
          "200": {"description": "OK", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/EmailList"}}}},
          "401": {"$ref": "#/components/responses/Error"},
          "403": {"$ref": "#/components/responses/Error"},
          "422": {"$ref": "#/components/responses/Error"}
        }
      }
    },
    "/emails/{id}": {
      "get": {
        "summary": "Retrieve an email",
        "description": "Requires the emails:read scope.",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "OK", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Email"}}}},
          "401": {"$ref": "#/components/responses/Error"},
          "403": {"$ref": "#/components/responses/Error"},
          "404": {"$ref": "#/components/responses/Error"}
        }
      }
    },
    "/events": {
      "get": {
        "summary": "List durable events",
        "description": "Requires webhooks:read. Lists immutable tenant events newest first. Public types are email.queued, email.delivered, email.delivery_delayed, email.failed, email.bounced, and email.suppressed (recipients were skipped by suppression policy; no SMTP attempt named them). delivered means final SMTP DATA was accepted, not inbox placement.",
        "parameters": [
          {"name":"limit","in":"query","schema":{"type":"integer","minimum":1,"maximum":100,"default":20}},
          {"name":"cursor","in":"query","schema":{"type":"string"}}
        ],
        "responses": {"200":{"description":"OK","content":{"application/json":{"schema":{"$ref":"#/components/schemas/EventList"}}}},"401":{"$ref":"#/components/responses/Error"},"403":{"$ref":"#/components/responses/Error"}}
      }
    },
    "/suppressions": {
      "post": {
        "summary": "Suppress a recipient address",
        "description": "Requires suppressions:write. Adds the address to THIS account's suppression list: future emails will not be sent to it (checked at acceptance and again immediately before delivery). Suppression is tenant-scoped policy, not delivery history. The address is canonicalized: whitespace and one <> pair removed, ASCII only, domain lower-cased, and the local part lower-cased FOR MATCHING ONLY (the address is still sent exactly as you supplied it; RFC 5321 discourages case-sensitive local parts and treating case variants as one recipient is the safe direction for a deny list). Dots and +tags are significant and are NEVER folded: john.smith@gmail.com and johnsmith@gmail.com are different addresses. Only reason 'manual' can be created through the API (hard_bounce entries are created automatically from delivery outcomes; complaint and unsubscribe are reserved and not produced by MailX yet). Idempotent: suppressing an already-suppressed address returns the existing entry unchanged with 200 (201 when newly created), so retries and concurrent requests are safe. 422 invalid_email or invalid_reason for bad input.",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/CreateSuppressionRequest"}}}},
        "responses": {
          "201": {"description": "Suppression created", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Suppression"}}}},
          "200": {"description": "Already suppressed; the existing entry is returned unchanged", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Suppression"}}}},
          "400": {"$ref": "#/components/responses/Error"}, "401": {"$ref": "#/components/responses/Error"}, "403": {"$ref": "#/components/responses/Error"},
          "415": {"$ref": "#/components/responses/Error"}, "422": {"$ref": "#/components/responses/Error"}, "500": {"$ref": "#/components/responses/Error"}
        }
      },
      "get": {
        "summary": "List suppressed recipients",
        "description": "Requires suppressions:read. Lists this account's entries newest first with keyset pagination (limit, cursor). The optional 'email' filter matches one address after the same canonicalization as creation. Other accounts' entries are never visible.",
        "parameters": [
          {"name": "limit", "in": "query", "schema": {"type": "integer", "minimum": 1, "maximum": 100, "default": 20}},
          {"name": "cursor", "in": "query", "schema": {"type": "string"}},
          {"name": "email", "in": "query", "schema": {"type": "string"}}
        ],
        "responses": {
          "200": {"description": "A page of suppressions", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/SuppressionList"}}}},
          "400": {"$ref": "#/components/responses/Error"}, "401": {"$ref": "#/components/responses/Error"}, "403": {"$ref": "#/components/responses/Error"},
          "422": {"$ref": "#/components/responses/Error"}, "500": {"$ref": "#/components/responses/Error"}
        }
      }
    },
    "/suppressions/{id}": {
      "get": {
        "summary": "Get a suppression",
        "description": "Requires suppressions:read. Another account's entry is indistinguishable from a missing one (404).",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "The suppression", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Suppression"}}}},
          "401": {"$ref": "#/components/responses/Error"}, "403": {"$ref": "#/components/responses/Error"},
          "404": {"$ref": "#/components/responses/Error"}, "500": {"$ref": "#/components/responses/Error"}
        }
      },
      "delete": {
        "summary": "Remove a suppression (unsuppress)",
        "description": "Requires suppressions:write. Hard-deletes the entry: FUTURE emails to the address may be attempted again. It changes no history: past messages, delivery attempts and events are untouched, a message that ended as 'suppressed' stays terminal, and nothing is re-queued or re-sent. To send again you must submit a new email.",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "204": {"description": "Removed"},
          "401": {"$ref": "#/components/responses/Error"}, "403": {"$ref": "#/components/responses/Error"},
          "404": {"$ref": "#/components/responses/Error"}, "500": {"$ref": "#/components/responses/Error"}
        }
      }
    },
    "/webhooks": {
      "post": {
        "summary": "Create a webhook",
        "description": "Requires webhooks:write. Returns signing_secret exactly once. MailX signs timestamp + '.' + the exact raw JSON body with HMAC-SHA256 and sends MailX-Webhook-Id, MailX-Event-Id, MailX-Webhook-Timestamp, and MailX-Webhook-Signature (v1=<hex>). Consumers should reject timestamps older/newer than five minutes using a constant-time MAC comparison and deduplicate by event id. Delivery is asynchronous and at-least-once; webhook failure never changes email status.",
        "requestBody":{"required":true,"content":{"application/json":{"schema":{"$ref":"#/components/schemas/CreateWebhookRequest"}}}},
        "responses":{"201":{"description":"Created; secret visible only here","content":{"application/json":{"schema":{"$ref":"#/components/schemas/WebhookCreated"}}}},"401":{"$ref":"#/components/responses/Error"},"403":{"$ref":"#/components/responses/Error"},"415":{"$ref":"#/components/responses/Error"},"422":{"$ref":"#/components/responses/Error"}}
      },
      "get": {
        "summary":"List webhooks","description":"Requires webhooks:read. Signing secrets are never returned.",
        "parameters":[{"name":"limit","in":"query","schema":{"type":"integer","minimum":1,"maximum":100,"default":20}},{"name":"cursor","in":"query","schema":{"type":"string"}}],
        "responses":{"200":{"description":"OK","content":{"application/json":{"schema":{"$ref":"#/components/schemas/WebhookList"}}}},"401":{"$ref":"#/components/responses/Error"},"403":{"$ref":"#/components/responses/Error"}}
      }
    },
    "/webhooks/{id}": {
      "get":{"summary":"Retrieve a webhook","description":"Requires webhooks:read. Cross-tenant IDs return 404; the secret is omitted.","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"200":{"description":"OK","content":{"application/json":{"schema":{"$ref":"#/components/schemas/Webhook"}}}},"404":{"$ref":"#/components/responses/Error"}}},
      "delete":{"summary":"Disable a webhook","description":"Requires webhooks:write. Stops future fan-out and cancels unfinished deliveries while retaining history.","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"204":{"description":"Disabled"},"404":{"$ref":"#/components/responses/Error"}}}
    },
    "/webhooks/{id}/rotate-secret": {
      "post":{"summary":"Rotate a webhook secret","description":"Requires webhooks:write. New delivery claims use the replacement secret immediately and the new raw secret is returned once. A request already in flight may complete with the previous secret, so consumers should allow a short overlap during planned rotation.","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"200":{"description":"Rotated","content":{"application/json":{"schema":{"$ref":"#/components/schemas/WebhookCreated"}}}},"404":{"$ref":"#/components/responses/Error"}}}
    },
    "/webhooks/{id}/deliveries": {
      "get":{"summary":"List webhook deliveries","description":"Requires webhooks:read. Uses opaque keyset pagination. Delivery is at-least-once: consumers must deduplicate by event_id. Any 2xx succeeds; network errors, timeouts, 408, 429, and 5xx retry with capped exponential jitter (maximum 8 attempts, Retry-After capped at one hour); redirects and ordinary 4xx are terminal. Replay is not implemented in v0.22. Delivery order is not guaranteed across endpoints or retries.","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}},{"name":"limit","in":"query","schema":{"type":"integer","minimum":1,"maximum":100,"default":20}},{"name":"cursor","in":"query","schema":{"type":"string"}}],"responses":{"200":{"description":"OK","content":{"application/json":{"schema":{"$ref":"#/components/schemas/WebhookDeliveryList"}}}},"404":{"$ref":"#/components/responses/Error"}}}
    },
    "/domains": {
      "post": {
        "summary": "Add a domain",
        "description": "Requires domains:write. Creates a pending DNS-ownership resource and returns the TXT record to publish. Ownership verification is not DKIM, SPF, DMARC, or a guarantee of inbox placement.",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/CreateDomainRequest"}}}},
        "responses": {
          "201": {"description": "Created", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Domain"}}}},
          "401": {"$ref": "#/components/responses/Error"}, "403": {"$ref": "#/components/responses/Error"},
          "409": {"$ref": "#/components/responses/Error"}, "415": {"$ref": "#/components/responses/Error"},
          "422": {"$ref": "#/components/responses/Error"}
        }
      },
      "get": {
        "summary": "List domains",
        "description": "Requires domains:read. Returns only the authenticated tenant's active domain resources.",
        "parameters": [
          {"name": "limit", "in": "query", "schema": {"type": "integer", "minimum": 1, "maximum": 100, "default": 20}},
          {"name": "cursor", "in": "query", "schema": {"type": "string"}, "description": "Opaque cursor from next_cursor."}
        ],
        "responses": {
          "200": {"description": "OK", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/DomainList"}}}},
          "400": {"$ref": "#/components/responses/Error"}, "401": {"$ref": "#/components/responses/Error"},
          "403": {"$ref": "#/components/responses/Error"}, "422": {"$ref": "#/components/responses/Error"},
          "500": {"$ref": "#/components/responses/Error"}
        }
      }
    },
    "/domains/{id}": {
      "get": {
        "summary": "Retrieve a domain",
        "description": "Requires domains:read. A resource owned by another tenant is reported as 404.",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "OK", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Domain"}}}},
          "401": {"$ref": "#/components/responses/Error"}, "403": {"$ref": "#/components/responses/Error"}, "404": {"$ref": "#/components/responses/Error"}
        }
      },
      "delete": {
        "summary": "Remove a domain",
        "description": "Requires domains:write. Soft-deletes the resource without deleting message history.",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "204": {"description": "Removed"}, "401": {"$ref": "#/components/responses/Error"},
          "403": {"$ref": "#/components/responses/Error"}, "404": {"$ref": "#/components/responses/Error"}
        }
      }
    },
    "/domains/{id}/verify": {
      "post": {
        "summary": "Verify domain ownership",
        "description": "Requires domains:write. MailX performs a bounded public DNS TXT lookup. A missing or wrong record returns the resource still pending; temporary DNS infrastructure failure returns 503. Verified ownership is monotonic in v0.21 and repeated calls are idempotent.",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "Current ownership state", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Domain"}}}},
          "401": {"$ref": "#/components/responses/Error"}, "403": {"$ref": "#/components/responses/Error"},
          "404": {"$ref": "#/components/responses/Error"}, "409": {"$ref": "#/components/responses/Error"},
          "503": {"$ref": "#/components/responses/Error"}
        }
      }
    },
    "/domains/{id}/dkim": {
      "get": {
        "summary": "DKIM status for a domain",
        "description": "Requires domains:read. Returns the domain's DKIM keys (public metadata and the DNS TXT record to publish). The private key is never returned by any endpoint. 'signing' is true when an active key exists; from then on mail from this domain is always signed (a key failure refuses the send instead of sending unsigned).",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "DKIM state", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/DkimStatus"}}}},
          "401": {"$ref": "#/components/responses/Error"}, "403": {"$ref": "#/components/responses/Error"},
          "404": {"$ref": "#/components/responses/Error"}, "500": {"$ref": "#/components/responses/Error"}, "503": {"$ref": "#/components/responses/Error"}
        }
      },
      "post": {
        "summary": "Generate a DKIM key (setup or rotation)",
        "description": "Requires domains:write and a VERIFIED domain (409 domain_not_verified otherwise). Generates a 2048-bit rsa-sha256 key with a fresh selector and stores it as 'pending'; a pending key does not sign. Publish the returned TXT record, then call verify. If an active key exists this starts a rotation: the active key keeps signing until the new one is verified. Only one pending key may exist (409 dkim_key_pending).",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "201": {"description": "Key created (pending)", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/DkimStatus"}}}},
          "401": {"$ref": "#/components/responses/Error"}, "403": {"$ref": "#/components/responses/Error"},
          "404": {"$ref": "#/components/responses/Error"}, "409": {"$ref": "#/components/responses/Error"},
          "500": {"$ref": "#/components/responses/Error"}, "503": {"$ref": "#/components/responses/Error"}
        }
      }
    },
    "/domains/{id}/dkim/verify": {
      "post": {
        "summary": "Verify DKIM DNS publication and activate the pending key",
        "description": "Requires domains:write. Performs a bounded public DNS TXT lookup for the pending key's selector and compares the published public key. On a match the pending key becomes active and the previous active key is retired (its private key destroyed); keep the old DNS record published for at least 24 hours so messages queued before rotation still verify. 'published:false' means the record is absent or different and nothing changed. 409 dkim_no_pending_key when there is nothing to verify; 503 on DNS infrastructure failure.",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "Verification result", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/DkimVerifyResult"}}}},
          "401": {"$ref": "#/components/responses/Error"}, "403": {"$ref": "#/components/responses/Error"},
          "404": {"$ref": "#/components/responses/Error"}, "409": {"$ref": "#/components/responses/Error"},
          "500": {"$ref": "#/components/responses/Error"}, "503": {"$ref": "#/components/responses/Error"}
        }
      }
    },
    "/domains/{id}/dmarc": {
      "get": {
        "summary": "DMARC guidance and alignment model for a domain",
        "description": "Requires domains:read. NO DNS query is made ('checked' is false, readiness 'unchecked'). Returns the DMARC record MailX recommends and how MailX's DKIM and SPF identities relate to the From domain. DMARC (RFC 9989, which obsoletes RFC 7489) lives in a TXT record at _dmarc.<domain> and ties the visible RFC 5322 From domain to authenticated identities: a message passes DMARC at a receiver when SPF or DKIM passes AND the authenticated domain aligns with the From domain. Alignment is 'relaxed' (same Organizational Domain, determined by the RFC 9989 DNS Tree Walk, never by suffix matching or a public-suffix list) or 'strict' (identical domain) per the record's adkim/aspf tags (default relaxed). In MailX the From domain, the SPF/MAIL FROM domain and the DKIM d= domain are the same tenant-verified domain, so both paths align exactly. The recommended first record is 'v=DMARC1; p=none', a monitoring policy: MailX never chooses enforcement for you and never adds rua/ruf report addresses (MailX has no report intake). Tighten to quarantine or reject yourself once you have confirmed all your legitimate senders authenticate and align. DMARC does not grant sending rights (domain ownership does), does not change DKIM keys or SPF authorization, and does not block or slow sending in MailX.",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "DMARC guidance", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/DmarcStatus"}}}},
          "401": {"$ref": "#/components/responses/Error"}, "403": {"$ref": "#/components/responses/Error"},
          "404": {"$ref": "#/components/responses/Error"}, "500": {"$ref": "#/components/responses/Error"}, "503": {"$ref": "#/components/responses/Error"}
        }
      }
    },
    "/domains/{id}/dmarc/verify": {
      "post": {
        "summary": "Inspect the domain's published DMARC policy and MailX's alignment readiness",
        "description": "Requires domains:write and a VERIFIED domain (409 domain_not_verified otherwise, with no DNS query). Performs the RFC 9989 DNS Tree Walk with bounded public TXT lookups (5 s total, at most 8 queries per walked domain, at most 64 records per answer, 2048-byte record): _dmarc.<domain>, then each parent down to the TLD (shortened to 7 labels for names with 8 or more), stopping at a single record carrying psd=y or psd=n. That one walk yields both the governing policy (the record at the domain, else its Organizational Domain, else its Public Suffix Domain, with sp applying to subdomains) and the Organizational Domain that relaxed alignment uses ('organizational_domain'); it is only known after this call. Multiple records at one name are all discarded and the walk continues, but MailX reports 'conflict' when that happens at the domain itself. A DNS error anywhere in the walk yields 'temporary_error' with nothing concluded (no parent policy is substituted and readiness is 'unknown'). It also runs one SPF verification. 'dns.status': 'monitoring' (valid, effective policy none), 'enforcing' (valid, quarantine or reject), 'not_configured', 'conflict' (more than one DMARC record at one name: RFC 9989 makes receivers discard all of them, so fix it), 'invalid' (malformed, duplicate or invalid tags, or no p and no valid rua; 'reason' is a code), 'temporary_error' (DNS failed or timed out; nothing is concluded, and it is never treated as a misconfiguration). An existing policy is never rewritten and MailX never recommends a second record. A record found at the Organizational Domain is reported with source 'organizational_domain' (or 'public_suffix_domain' for a psd=y record above it) and its sp policy (when present) applies to this subdomain; an intermediate record that is neither the domain, its Organizational Domain nor its PSD does not govern. Tags: v, p, sp, np, adkim, aspf, t, psd, rua, ruf are understood; pct, rf and ri were removed by RFC 9989 and are ignored with a warning; unknown tags are ignored. 'readiness': 'ready' means a valid DMARC record exists and at least one authentication path is configured AND aligned (DKIM: an active key; SPF: a verified SPF record in direct mode), 'dns_action_required', 'authentication_incomplete', or 'unknown' (for example relay mode, where the relay may rewrite the return-path so the SPF path cannot be established, or a DNS/SPF/DKIM lookup failure). READINESS IS NOT A RECEIVER RESULT: 'ready' is a precondition for a receiver to pass DMARC, never proof that it did (receiver_result is always 'not_observed'); a published p=reject does not mean mail reaches the inbox. Limits: relaxed alignment between two different domains needs an additional tree walk for the second domain (in MailX both identities equal the From domain, so none is needed); aggregate and failure reports are not received or shown. This call stores nothing and changes no domain, DKIM, SPF or sending-authorization state. MailX does not block sending when DMARC is missing or invalid.",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "Verification result", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/DmarcStatus"}}}},
          "401": {"$ref": "#/components/responses/Error"}, "403": {"$ref": "#/components/responses/Error"},
          "404": {"$ref": "#/components/responses/Error"}, "409": {"$ref": "#/components/responses/Error"},
          "500": {"$ref": "#/components/responses/Error"}, "503": {"$ref": "#/components/responses/Error"}
        }
      }
    },
    "/domains/{id}/spf": {
      "get": {
        "summary": "SPF guidance for a domain",
        "description": "Requires domains:read. Returns the SPF record MailX recommends for this domain. NO DNS query is made ('checked' is false, status 'unchecked'); call the verify endpoint to compare against public DNS. SPF is checked by receivers against the envelope sender (MAIL FROM) domain, which for MailX is the From-address domain. Direct mode (default): MailX connects to recipient servers from its own public IP addresses, declared by the operator; 'sending.ips' lists them and the record authorizes exactly those addresses with ip4/ip6. Relay mode: all mail goes through the operator's trusted relay; MailX cannot know the relay's SPF requirements, so it only knows an include domain if the operator configured one, otherwise 'expected.action' is 'follow_relay_provider' and no record is invented. If the operator has not declared the sending addresses, status is 'sending_infrastructure_unknown' and no record is generated. Publish ONE SPF TXT record per domain: if you already have one, use the 'update_existing' value from verify, which extends it instead of creating a second. SPF does not authorize sending from a domain in MailX (domain ownership does), does not affect DKIM, and does not guarantee delivery or inbox placement.",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "SPF guidance", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/SpfStatus"}}}},
          "401": {"$ref": "#/components/responses/Error"}, "403": {"$ref": "#/components/responses/Error"},
          "404": {"$ref": "#/components/responses/Error"}, "500": {"$ref": "#/components/responses/Error"}, "503": {"$ref": "#/components/responses/Error"}
        }
      }
    },
    "/domains/{id}/spf/verify": {
      "post": {
        "summary": "Check the domain's published SPF record",
        "description": "Requires domains:write and a VERIFIED domain (409 domain_not_verified otherwise, and no DNS query is made). Performs one bounded public TXT lookup (5 s timeout, at most 64 TXT records, 2048-byte SPF record) and reports: 'verified' (the single published record literally authorizes MailX's sending addresses via ip4/ip6, or, in relay mode, contains the configured include); 'not_configured' (no SPF record); 'mismatch' (a record exists but does not authorize MailX; 'expected.value' is your existing record extended with the missing mechanisms, still ONE record); 'conflict' (more than one SPF record, which receivers treat as a permanent error: merge them into one); 'invalid' (malformed or over the size bounds; 'reason' is a code); 'temporary_error' (DNS failed or timed out; nothing is concluded, retry later, and it is not a misconfiguration); 'sending_infrastructure_unknown' (see the GET endpoint). Limits: include, a, mx, exists, ptr and redirect terms are parsed but NOT followed (RFC 7208 caps evaluation at 10 DNS-querying terms and MailX does not walk DNS trees), so a record that authorizes MailX only through such terms reports 'mismatch' with 'unevaluated_mechanisms' true; 'verified' is never a claim about how a particular receiver will evaluate the record. This call stores nothing and changes no domain, DKIM or sending-authorization state. MailX does not block sending when SPF is missing or unverified.",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "Verification result", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/SpfStatus"}}}},
          "401": {"$ref": "#/components/responses/Error"}, "403": {"$ref": "#/components/responses/Error"},
          "404": {"$ref": "#/components/responses/Error"}, "409": {"$ref": "#/components/responses/Error"},
          "500": {"$ref": "#/components/responses/Error"}, "503": {"$ref": "#/components/responses/Error"}
        }
      }
    }
  },
  "components": {
    "securitySchemes": {
      "ApiKeyAuth": {
        "type": "http",
        "scheme": "bearer",
        "description": "A MailX API key (format mx_<key_id>_<secret>), created via the mailx CLI - NOT a JWT. Paste the raw key (including the mx_ prefix) into Swagger UI's Authorize button to try requests here."
      }
    },
    "responses": {
      "Error": {"description": "Error", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/APIError"}}}}
    },
    "schemas": {
      "SendEmailRequest": {
        "type": "object",
        "required": ["from", "to"],
        "properties": {
          "from": {"type": "string", "example": "Feranmi <hello@example.com>"},
          "to": {"type": "array", "items": {"type": "string"}, "maxItems": 50, "example": ["user@example.com"]},
          "cc": {"type": "array", "items": {"type": "string"}},
          "bcc": {"type": "array", "items": {"type": "string"}},
          "reply_to": {"type": "string"},
          "subject": {"type": "string", "maxLength": 500},
          "html": {"type": "string", "description": "At least one of html/text is required."},
          "text": {"type": "string"},
          "scheduled_at": {"type": "string", "format": "date-time", "nullable": true, "description": "RFC 3339. Omit to send immediately."}
        },
        "additionalProperties": false
      },
      "Email": {
        "type": "object",
        "properties": {
          "id": {"type": "string"},
          "from": {"type": "string"},
          "to": {"type": "array", "items": {"type": "string"}},
          "cc": {"type": "array", "items": {"type": "string"}},
          "bcc": {"type": "array", "items": {"type": "string"}, "description": "Only ever returned to the sending tenant retrieving their own message - never present in the delivered MIME."},
          "reply_to": {"type": "string"},
          "subject": {"type": "string"},
          "html": {"type": "string", "nullable": true, "description": "Only populated by GET /v1/emails/{id}, never by the list endpoint."},
          "text": {"type": "string", "nullable": true},
          "status": {"type": "string", "enum": ["queued", "processing", "retrying", "delivered", "failed", "bounced", "suppressed"]},
          "created_at": {"type": "string", "format": "date-time"},
          "queued_at": {"type": "string", "format": "date-time", "nullable": true},
          "delivered_at": {"type": "string", "format": "date-time", "nullable": true}
        }
      },
      "EmailList": {
        "type": "object",
        "properties": {
          "data": {"type": "array", "items": {"$ref": "#/components/schemas/Email"}},
          "next_cursor": {"type": "string", "nullable": true}
        }
      },
      "CreateDomainRequest": {
        "type": "object", "required": ["name"], "additionalProperties": false,
        "properties": {"name": {"type": "string", "example": "example.com", "description": "Public ASCII DNS name. Root and subdomain resources are independent; wildcards, IPs, IDNs, and public suffixes are rejected in v0.21."}}
      },
      "DkimKey": {
        "type": "object", "required": ["selector", "algorithm", "key_bits", "status", "created_at", "dns"],
        "properties": {
          "selector": {"type": "string", "example": "mx202609211a2b"},
          "algorithm": {"type": "string", "enum": ["rsa-sha256"]},
          "key_bits": {"type": "integer", "example": 2048},
          "status": {"type": "string", "enum": ["pending", "active", "retired"]},
          "created_at": {"type": "string", "format": "date-time"},
          "activated_at": {"type": "string", "format": "date-time", "nullable": true},
          "retired_at": {"type": "string", "format": "date-time", "nullable": true},
          "dns": {"type": "object", "required": ["type", "name", "value", "value_chunks"], "properties": {
            "type": {"type": "string", "enum": ["TXT"]},
            "name": {"type": "string", "example": "mx202609211a2b._domainkey.example.com"},
            "value": {"type": "string", "example": "v=DKIM1; k=rsa; p=<base64-public-key>"},
            "value_chunks": {"type": "array", "items": {"type": "string"}, "description": "The value split into DNS character-strings of at most 255 bytes, for providers that require it."}
          }}
        }
      },
      "DkimStatus": {
        "type": "object", "required": ["domain_id", "domain", "signing", "active", "pending", "retired"],
        "properties": {
          "domain_id": {"type": "string"}, "domain": {"type": "string"},
          "signing": {"type": "boolean", "description": "True when an active key exists; mail from the domain is then always DKIM-signed."},
          "active": {"allOf": [{"$ref": "#/components/schemas/DkimKey"}], "nullable": true},
          "pending": {"allOf": [{"$ref": "#/components/schemas/DkimKey"}], "nullable": true},
          "retired": {"type": "array", "items": {"$ref": "#/components/schemas/DkimKey"}}
        }
      },
      "DkimVerifyResult": {
        "type": "object", "required": ["published", "status"],
        "properties": {"published": {"type": "boolean"}, "status": {"$ref": "#/components/schemas/DkimStatus"}}
      },
      "SpfStatus": {
        "type": "object", "required": ["domain_id", "domain", "mode", "checked", "status", "sending", "expected", "warnings", "unevaluated_mechanisms"],
        "properties": {
          "domain_id": {"type": "string"}, "domain": {"type": "string"},
          "mode": {"type": "string", "enum": ["direct", "relay"], "description": "How this MailX deployment reaches the Internet."},
          "checked": {"type": "boolean", "description": "True only when a public DNS lookup was performed for this response."},
          "status": {"type": "string", "enum": ["unchecked", "verified", "not_configured", "mismatch", "conflict", "invalid", "temporary_error", "sending_infrastructure_unknown"]},
          "reason": {"type": "string", "description": "Bounded reason code (for example sending_address_not_listed, relay_include_missing, multiple_spf_records, bad_cidr, record_too_long, dns_error). Never contains DNS text."},
          "sending": {"type": "object", "properties": {
            "ips": {"type": "array", "items": {"type": "string"}, "description": "Public sending addresses declared by the operator (direct mode)."},
            "relay_include": {"type": "string", "description": "Include domain declared by the operator for the relay (relay mode)."}}},
          "published_record": {"type": "string", "description": "The single SPF record found in your DNS (only present when exactly one was found and it parsed)."},
          "expected": {"type": "object", "required": ["action"], "properties": {
            "action": {"type": "string", "enum": ["create", "update_existing", "none", "merge_records", "fix_record", "declare_sending_ips", "follow_relay_provider"]},
            "type": {"type": "string", "enum": ["TXT"]}, "name": {"type": "string"},
            "value": {"type": "string", "description": "One complete SPF record. Empty when MailX cannot truthfully provide one."}}},
          "warnings": {"type": "array", "items": {"type": "string", "enum": ["unevaluated_mechanisms", "permits_all", "dns_lookup_limit_risk", "deprecated_ptr"]}},
          "unevaluated_mechanisms": {"type": "boolean", "description": "True when the record has include/a/mx/exists/ptr/redirect terms MailX did not follow."}
        }
      },
      "DmarcPath": {
        "type": "object", "required": ["identity", "aligned", "mode", "status"],
        "properties": {
          "identity": {"type": "string", "description": "The authenticated domain MailX would use: the DKIM d= domain, or the SPF MAIL FROM domain (empty when it cannot be established, for example relay mode)."},
          "aligned": {"type": "boolean", "description": "Whether that identity aligns with the From domain under the mode. This is a relation between two names, not an authentication result."},
          "mode": {"type": "string", "enum": ["relaxed", "strict"]},
          "status": {"type": "string", "enum": ["ready", "not_configured", "not_aligned", "unknown"]},
          "reason": {"type": "string", "description": "Bounded code, for example no_active_dkim_key, spf_not_verified, relay_return_path_unknown, spf_state_unavailable, not_checked."}
        }
      },
      "DmarcStatus": {
        "type": "object", "required": ["domain_id", "domain", "checked", "readiness", "receiver_result", "dns", "dkim", "spf", "expected", "warnings"],
        "properties": {
          "domain_id": {"type": "string"}, "domain": {"type": "string"}, "organizational_domain": {"type": "string", "description": "The Organizational Domain from the RFC 9989 DNS Tree Walk; only present after a verify call (it cannot be known without DNS)."},
          "mode": {"type": "string", "enum": ["direct", "relay"], "description": "MailX's routing mode (see the SPF endpoints)."},
          "checked": {"type": "boolean", "description": "True only when public DNS was queried for this response."},
          "readiness": {"type": "string", "enum": ["unchecked", "ready", "dns_action_required", "authentication_incomplete", "unknown"], "description": "Sender-side precondition for DMARC, not a receiver result."},
          "receiver_result": {"type": "string", "enum": ["not_observed"], "description": "MailX does not observe receivers' DMARC results and never claims one."},
          "dns": {"type": "object", "required": ["status", "testing", "report_uri_count"], "properties": {
            "status": {"type": "string", "enum": ["unchecked", "not_configured", "monitoring", "enforcing", "conflict", "invalid", "temporary_error"]},
            "reason": {"type": "string"}, "source": {"type": "string", "enum": ["domain", "organizational_domain", "public_suffix_domain"]},
            "record_name": {"type": "string", "description": "The _dmarc. name where the governing record was found."},
            "policy": {"type": "string", "enum": ["none", "quarantine", "reject"]}, "subdomain_policy": {"type": "string", "enum": ["none", "quarantine", "reject"]},
            "effective_policy": {"type": "string", "enum": ["none", "quarantine", "reject"], "description": "The policy that applies to this domain (sp when the record is at the Organizational Domain or PSD)."},
            "testing": {"type": "boolean", "description": "The t=y testing flag."}, "report_uri_count": {"type": "integer", "description": "Usable mailto: rua addresses; MailX cannot receive the reports."},
            "published_record": {"type": "string", "description": "The single DMARC record found."}}},
          "dkim": {"$ref": "#/components/schemas/DmarcPath"}, "spf": {"$ref": "#/components/schemas/DmarcPath"},
          "expected": {"type": "object", "required": ["action"], "properties": {
            "action": {"type": "string", "enum": ["create", "none", "fix_record", "merge_records", "retry_later"]},
            "type": {"type": "string", "enum": ["TXT"]}, "name": {"type": "string"},
            "value": {"type": "string", "description": "Only set for create: 'v=DMARC1; p=none'. An existing policy is never rewritten."}}},
          "warnings": {"type": "array", "items": {"type": "string", "enum": ["deprecated_tag", "ignored_report_uri", "policy_defaulted_from_rua", "policy_not_enforcing", "testing_mode", "failure_reporting_enabled", "external_report_destination", "relay_may_alter_signed_content"]}}
        }
      },
      "CreateSuppressionRequest": {
        "type": "object", "required": ["email"], "additionalProperties": false,
        "properties": {
          "email": {"type": "string", "example": "person@example.com"},
          "reason": {"type": "string", "enum": ["manual"], "default": "manual", "description": "Only 'manual' may be created through the API."}
        }
      },
      "Suppression": {
        "type": "object", "required": ["id", "email", "reason", "source", "created_at"],
        "properties": {
          "id": {"type": "string"},
          "email": {"type": "string", "description": "The canonical suppression key (lower-cased)."},
          "reason": {"type": "string", "enum": ["manual", "hard_bounce", "complaint", "unsubscribe"], "description": "WHY. complaint and unsubscribe are reserved and not produced yet."},
          "source": {"type": "string", "enum": ["api", "delivery", "feedback"], "description": "HOW it was created, independent of the reason."},
          "message_id": {"type": "string", "description": "For hard_bounce: the email whose delivery produced it."},
          "smtp_code": {"type": "integer", "description": "For hard_bounce: the SMTP reply code."},
          "enhanced_status": {"type": "string", "description": "For hard_bounce: the RFC 3463 enhanced status (5.1.1 or 5.1.6)."},
          "created_at": {"type": "string", "format": "date-time"}
        }
      },
      "SuppressionList": {
        "type": "object", "required": ["data", "next_cursor"],
        "properties": {"data": {"type": "array", "items": {"$ref": "#/components/schemas/Suppression"}}, "next_cursor": {"type": "string", "nullable": true}}
      },
      "DNSRecord": {
        "type": "object", "required": ["type", "name", "value"],
        "properties": {
          "type": {"type": "string", "enum": ["TXT"]},
          "name": {"type": "string", "example": "_mailx-verification.example.com"},
          "value": {"type": "string", "example": "mailx-verification=example-token"}
        }
      },
      "Domain": {
        "type": "object", "required": ["id", "name", "ownership_state", "records", "created_at"],
        "properties": {
          "id": {"type": "string"}, "name": {"type": "string"},
          "ownership_state": {"type": "string", "enum": ["pending", "verified"], "description": "DNS ownership only; not deliverability readiness."},
          "records": {"type": "array", "items": {"$ref": "#/components/schemas/DNSRecord"}},
          "created_at": {"type": "string", "format": "date-time"},
          "verified_at": {"type": "string", "format": "date-time", "nullable": true},
          "last_checked_at": {"type": "string", "format": "date-time", "nullable": true}
        }
      },
      "DomainList": {
        "type": "object",
        "properties": {"data": {"type": "array", "items": {"$ref": "#/components/schemas/Domain"}}, "next_cursor": {"type": "string", "nullable": true}}
      },
      "CreateWebhookRequest": {
        "type":"object","required":["url","events"],"additionalProperties":false,
        "properties":{"url":{"type":"string","format":"uri","description":"Public HTTPS URL in production."},"events":{"type":"array","minItems":1,"items":{"type":"string","enum":["email.queued","email.delivered","email.delivery_delayed","email.failed","email.bounced","email.suppressed"]}}}
      },
      "Webhook": {
        "type":"object","required":["id","url","events","created_at","updated_at"],
        "properties":{"id":{"type":"string"},"url":{"type":"string"},"events":{"type":"array","items":{"type":"string"}},"created_at":{"type":"string","format":"date-time"},"updated_at":{"type":"string","format":"date-time"}}
      },
      "WebhookCreated": {
        "allOf":[{"$ref":"#/components/schemas/Webhook"},{"type":"object","required":["signing_secret"],"properties":{"signing_secret":{"type":"string","writeOnly":true,"description":"Shown only in this create/rotation response."}}}]
      },
      "WebhookList": {"type":"object","properties":{"data":{"type":"array","items":{"$ref":"#/components/schemas/Webhook"}},"next_cursor":{"type":"string","nullable":true}}},
      "WebhookDelivery": {
        "type":"object","required":["id","event_id","status","attempt_count","next_attempt_at","created_at"],
        "properties":{"id":{"type":"string"},"event_id":{"type":"string","description":"Stable across automatic attempts."},"status":{"type":"string","enum":["pending","delivering","succeeded","failed","cancelled"]},"attempt_count":{"type":"integer"},"next_attempt_at":{"type":"string","format":"date-time"},"last_error_category":{"type":"string"},"last_response_code":{"type":"integer"},"created_at":{"type":"string","format":"date-time"},"delivered_at":{"type":"string","format":"date-time","nullable":true},"failed_at":{"type":"string","format":"date-time","nullable":true}}
      },
      "WebhookDeliveryList": {"type":"object","properties":{"data":{"type":"array","items":{"$ref":"#/components/schemas/WebhookDelivery"}},"next_cursor":{"type":"string","nullable":true}}},
      "Event": {
        "type":"object","required":["id","type","api_version","created_at","data"],
        "description":"Stable logical event. Automatic webhook retries preserve this id.",
        "properties":{"id":{"type":"string"},"type":{"type":"string"},"api_version":{"type":"string","enum":["2026-09-01"]},"created_at":{"type":"string","format":"date-time"},"data":{"type":"object","properties":{"email_id":{"type":"string"}}}}
      },
      "EventList": {"type":"object","properties":{"data":{"type":"array","items":{"$ref":"#/components/schemas/Event"}},"next_cursor":{"type":"string","nullable":true}}},
      "APIError": {
        "type": "object",
        "properties": {
          "error": {
            "type": "object",
            "properties": {
              "type": {"type": "string", "enum": ["invalid_request", "validation_error", "authentication_error", "forbidden", "not_found", "conflict", "payload_too_large", "unsupported_media_type", "internal_error", "temporarily_unavailable"]},
              "code": {"type": "string"},
              "message": {"type": "string"},
              "request_id": {"type": "string"}
            }
          }
        }
      }
    }
  }
}`

func serveOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(openAPISpec))
}

// serveDocs renders Swagger UI (from a CDN, development-only) against
// /openapi.json so `docker compose up` gives an interactive contract with
// no build step.
const docsPage = `<!DOCTYPE html>
<html>
<head>
  <title>MailX API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    window.onload = () => SwaggerUIBundle({url: '/openapi.json', dom_id: '#swagger-ui'});
  </script>
</body>
</html>`

func serveDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write([]byte(docsPage))
}
