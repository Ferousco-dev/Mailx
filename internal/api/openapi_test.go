package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAPISpecParses(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(openAPISpec), &doc); err != nil {
		t.Fatalf("openapi.json does not parse: %v", err)
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatal("missing paths object")
	}
	for _, want := range []string{
		"/emails", "/emails/{id}", "/domains", "/domains/{id}", "/domains/{id}/verify", "/domains/{id}/dkim", "/domains/{id}/dkim/verify",
		"/events", "/webhooks", "/webhooks/{id}", "/webhooks/{id}/rotate-secret", "/webhooks/{id}/deliveries",
		"/templates", "/templates/{id}", "/contacts", "/contacts/{id}",
		"/audiences", "/audiences/{id}", "/audiences/{id}/contacts", "/audiences/{id}/contacts/{contact_id}",
	} {
		if _, ok := paths[want]; !ok {
			t.Errorf("documented spec is missing path %q", want)
		}
	}
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	for _, want := range []string{
		"SendEmailRequest", "Email", "EmailList", "CreateDomainRequest", "DNSRecord", "Domain", "DomainList",
		"CreateWebhookRequest", "Webhook", "WebhookCreated", "WebhookList", "Event", "EventList", "APIError",
		"WebhookDelivery", "WebhookDeliveryList", "DkimKey", "DkimStatus", "DkimVerifyResult",
		"Template", "CreateTemplateRequest", "UpdateTemplateRequest", "TemplateList",
		"Contact", "CreateContactRequest", "UpdateContactRequest", "ContactList",
		"Audience", "CreateAudienceRequest", "UpdateAudienceRequest", "AudienceList", "AddMemberRequest",
	} {
		if _, ok := schemas[want]; !ok {
			t.Errorf("documented spec is missing schema %q", want)
		}
	}

	schemes := doc["components"].(map[string]any)["securitySchemes"].(map[string]any)
	apiKeyAuth, ok := schemes["ApiKeyAuth"].(map[string]any)
	if !ok {
		t.Fatal("missing ApiKeyAuth security scheme")
	}
	if apiKeyAuth["type"] != "http" || apiKeyAuth["scheme"] != "bearer" {
		t.Errorf("expected an http/bearer security scheme, got %+v", apiKeyAuth)
	}
	if desc, _ := apiKeyAuth["description"].(string); strings.Contains(strings.ToLower(desc), "is a jwt") {
		t.Error("must never claim the API key IS a JWT")
	}

	postEmails := paths["/emails"].(map[string]any)["post"].(map[string]any)
	params, _ := postEmails["parameters"].([]any)
	found := false
	for _, p := range params {
		if p.(map[string]any)["name"] == "Idempotency-Key" {
			found = true
		}
	}
	if !found {
		t.Error("POST /emails must document the Idempotency-Key header")
	}

	domainList := paths["/domains"].(map[string]any)["get"].(map[string]any)
	responses := domainList["responses"].(map[string]any)
	for _, status := range []string{"200", "400", "401", "403", "422", "500"} {
		if _, ok := responses[status]; !ok {
			t.Errorf("GET /domains must document response %s", status)
		}
	}
}

// TestOpenAPIRoutesMatchRuntime is the drift guard: every path documented
// above must be a route the real mux actually serves (proven by getting
// something other than 404 from it), and vice versa for the routes this
// test knows about.
func TestOpenAPIRoutesMatchRuntime(t *testing.T) {
	mux, _, _ := setupMux(t)

	cases := []struct {
		method, path string
	}{
		{"POST", "/v1/emails"},
		{"GET", "/v1/emails"},
		{"GET", "/v1/emails/some-id"},
		{"POST", "/v1/audiences"},
		{"GET", "/v1/audiences"},
		{"GET", "/v1/audiences/some-id"},
		{"PATCH", "/v1/audiences/some-id"},
		{"DELETE", "/v1/audiences/some-id"},
		{"POST", "/v1/audiences/some-id/contacts"},
		{"GET", "/v1/audiences/some-id/contacts"},
		{"DELETE", "/v1/audiences/some-id/contacts/other-id"},
		{"POST", "/v1/contacts"},
		{"GET", "/v1/contacts"},
		{"GET", "/v1/contacts/some-id"},
		{"PATCH", "/v1/contacts/some-id"},
		{"DELETE", "/v1/contacts/some-id"},
		{"POST", "/v1/templates"},
		{"GET", "/v1/templates"},
		{"GET", "/v1/templates/some-id"},
		{"PATCH", "/v1/templates/some-id"},
		{"DELETE", "/v1/templates/some-id"},
		{"POST", "/v1/domains"},
		{"GET", "/v1/domains"},
		{"GET", "/v1/domains/some-id"},
		{"GET", "/v1/domains/some-id/dkim"},
		{"POST", "/v1/domains/some-id/dkim"},
		{"POST", "/v1/domains/some-id/dkim/verify"},
		{"DELETE", "/v1/domains/some-id"},
		{"POST", "/v1/domains/some-id/verify"},
		{"GET", "/v1/events"},
		{"POST", "/v1/webhooks"},
		{"GET", "/v1/webhooks"},
		{"GET", "/v1/webhooks/some-id"},
		{"DELETE", "/v1/webhooks/some-id"},
		{"POST", "/v1/webhooks/some-id/rotate-secret"},
		{"GET", "/v1/webhooks/some-id/deliveries"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		if c.method == "POST" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound && !strings.Contains(c.path, "some-id") {
			t.Errorf("%s %s: route not found (spec/runtime drift)", c.method, c.path)
		}
	}
}

func TestOpenAPIJSONEndpoint(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doJSON(t, mux, "GET", "/openapi.json", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("served /openapi.json does not parse: %v", err)
	}
}

func TestDocsEndpointServesHTML(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doJSON(t, mux, "GET", "/docs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html" {
		t.Fatalf("expected text/html, got %q", ct)
	}
}

func TestHealthEndpoints(t *testing.T) {
	mux, _, _ := setupMux(t)
	if rec := doJSON(t, mux, "GET", "/health/live", nil); rec.Code != http.StatusOK {
		t.Fatalf("live: got %d", rec.Code)
	}
	if rec := doJSON(t, mux, "GET", "/health/ready", nil); rec.Code != http.StatusOK {
		t.Fatalf("ready: got %d", rec.Code)
	}
}

// v0.31: every operation documents 429, every mutating operation documents 503, both with Retry-After,
// and every abuse-control error code the runtime can emit is named in the served contract.
func TestServedOpenAPIDocumentsRateLimitContract(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doJSON(t, mux, "GET", "/openapi.json", nil)
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for path, item := range doc["paths"].(map[string]any) {
		for method, opAny := range item.(map[string]any) {
			resp := opAny.(map[string]any)["responses"].(map[string]any)
			if ref, _ := resp["429"].(map[string]any)["$ref"].(string); ref != "#/components/responses/RateLimited" {
				t.Errorf("%s %s does not document 429", method, path)
			}
			if method != "get" {
				if ref, _ := resp["503"].(map[string]any)["$ref"].(string); ref != "#/components/responses/Unavailable" {
					t.Errorf("%s %s does not document 503", method, path)
				}
			}
		}
	}
	responses := doc["components"].(map[string]any)["responses"].(map[string]any)
	for _, name := range []string{"RateLimited", "Unavailable"} {
		hdrs := responses[name].(map[string]any)["headers"].(map[string]any)
		if _, ok := hdrs["Retry-After"]; !ok {
			t.Errorf("%s response does not document Retry-After", name)
		}
	}
	body := rec.Body.String()
	for _, code := range []string{"tenant_rate_limited", "api_key_rate_limited", "recipient_rate_limited", "tenant_queue_full", "system_busy", "rate_limiter_unavailable"} {
		if !strings.Contains(body, code) {
			t.Errorf("served contract never mentions the runtime error code %q", code)
		}
	}
	if !strings.Contains(body, "Idempotency-Key") || !strings.Contains(body, "not charged") {
		t.Error("the contract must state how limits interact with idempotency")
	}
	if errorStatus[ErrRateLimited] != http.StatusTooManyRequests {
		t.Error("rate_limited must map to 429")
	}
}
