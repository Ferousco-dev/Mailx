package api

import "net/http"

// openAPISpec is hand-written (contract-first), not generated from Go
// types/routes. Rationale (see the v0.18 report's "OpenAPI architecture"
// section): the public JSON schema is a deliberate product surface with
// its own naming/shape decisions (e.g. "html"/"text", not Go field
// names), and MailX has exactly three routes today — a generator would
// buy safety against drift at the cost of a dependency this milestone
// does not otherwise need. openapi_test.go keeps it from silently
// diverging from the real handlers by asserting the documented routes
// and schemas match runtime behavior.
const openAPISpec = `{
  "openapi": "3.0.3",
  "info": {
    "title": "MailX API",
    "version": "v1",
    "description": "MailX's developer-facing email API. Authentication is not yet implemented (see v0.19) - every request in this development build is attributed to one fixed development tenant."
  },
  "servers": [{"url": "/v1"}],
  "paths": {
    "/emails": {
      "post": {
        "summary": "Send an email",
        "description": "Durably accepts an email for asynchronous processing. All to/cc/bcc recipients must share one delivery domain; mixed-domain requests receive 422 before acceptance. 202 means MailX has validated and durably recorded the email and has durable responsibility for eventually attempting delivery - it does NOT mean the email has been delivered, that the recipient's server accepted it, or that Redis currently has the job.",
        "requestBody": {
          "required": true,
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/SendEmailRequest"}}}
        },
        "responses": {
          "202": {"description": "Accepted", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Email"}}}},
          "400": {"$ref": "#/components/responses/Error"},
          "413": {"$ref": "#/components/responses/Error"},
          "415": {"$ref": "#/components/responses/Error"},
          "422": {"$ref": "#/components/responses/Error"},
          "500": {"$ref": "#/components/responses/Error"}
        }
      },
      "get": {
        "summary": "List emails",
        "parameters": [
          {"name": "limit", "in": "query", "schema": {"type": "integer", "minimum": 1, "maximum": 100, "default": 20}},
          {"name": "cursor", "in": "query", "schema": {"type": "string"}, "description": "Opaque; from a previous response's next_cursor. Never construct one."},
          {"name": "status", "in": "query", "schema": {"type": "string", "enum": ["queued", "processing", "retrying", "delivered", "failed", "bounced"]}}
        ],
        "responses": {
          "200": {"description": "OK", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/EmailList"}}}},
          "422": {"$ref": "#/components/responses/Error"}
        }
      }
    },
    "/emails/{id}": {
      "get": {
        "summary": "Retrieve an email",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "OK", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Email"}}}},
          "404": {"$ref": "#/components/responses/Error"}
        }
      }
    }
  },
  "components": {
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
      "APIError": {
        "type": "object",
        "properties": {
          "error": {
            "type": "object",
            "properties": {
              "type": {"type": "string", "enum": ["invalid_request", "validation_error", "not_found", "conflict", "payload_too_large", "unsupported_media_type", "internal_error", "temporarily_unavailable"]},
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
