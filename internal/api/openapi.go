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
        "description": "Requires the emails:send scope. Durably accepts an email for asynchronous processing. All to/cc/bcc recipients must share one delivery domain; mixed-domain requests receive 422 before acceptance. 202 means MailX has validated and durably recorded the email and has durable responsibility for eventually attempting delivery - it does NOT mean the email has been delivered, that the recipient's server accepted it, or that Redis currently has the job. Retrying safely: supply the same Idempotency-Key on retry to get the original result back instead of creating a second email; this prevents duplicate MailX email SUBMISSIONS from a repeated HTTP request - it does not and cannot guarantee exactly-once SMTP delivery to the recipient's server. v0.21 records DNS ownership but does not yet enforce a verified From domain; enforcement is deferred to the sending-identity/DKIM milestone.",
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
          "500": {"$ref": "#/components/responses/Error"}
        }
      },
      "get": {
        "summary": "List emails",
        "description": "Requires the emails:read scope.",
        "parameters": [
          {"name": "limit", "in": "query", "schema": {"type": "integer", "minimum": 1, "maximum": 100, "default": 20}},
          {"name": "cursor", "in": "query", "schema": {"type": "string"}, "description": "Opaque; from a previous response's next_cursor. Never construct one."},
          {"name": "status", "in": "query", "schema": {"type": "string", "enum": ["queued", "processing", "retrying", "delivered", "failed", "bounced"]}}
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
        "description": "Requires webhooks:read. Lists immutable tenant events newest first. Public types are email.queued, email.delivered, email.delivery_delayed, email.failed, and email.bounced. delivered means final SMTP DATA was accepted, not inbox placement.",
        "parameters": [
          {"name":"limit","in":"query","schema":{"type":"integer","minimum":1,"maximum":100,"default":20}},
          {"name":"cursor","in":"query","schema":{"type":"string"}}
        ],
        "responses": {"200":{"description":"OK","content":{"application/json":{"schema":{"$ref":"#/components/schemas/EventList"}}}},"401":{"$ref":"#/components/responses/Error"},"403":{"$ref":"#/components/responses/Error"}}
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
          "status": {"type": "string", "enum": ["queued", "processing", "retrying", "delivered", "failed", "bounced"]},
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
        "properties":{"url":{"type":"string","format":"uri","description":"Public HTTPS URL in production."},"events":{"type":"array","minItems":1,"items":{"type":"string","enum":["email.queued","email.delivered","email.delivery_delayed","email.failed","email.bounced"]}}}
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
