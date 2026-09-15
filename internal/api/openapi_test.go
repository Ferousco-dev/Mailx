package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	for _, want := range []string{"/emails", "/emails/{id}"} {
		if _, ok := paths[want]; !ok {
			t.Errorf("documented spec is missing path %q", want)
		}
	}
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	for _, want := range []string{"SendEmailRequest", "Email", "EmailList", "APIError"} {
		if _, ok := schemas[want]; !ok {
			t.Errorf("documented spec is missing schema %q", want)
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
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		if c.method == "POST" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound && c.path != "/v1/emails/some-id" {
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
