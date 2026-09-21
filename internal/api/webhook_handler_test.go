package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/webhook"
)

func setupWebhookAPI(t *testing.T, scopes []string) (http.Handler, *database.DB, database.Tenant, *webhook.Service) {
	t.Helper()
	db := newTestDB(t)
	tenant := newTestTenant(t, db)
	authSvc := auth.NewService(db, nil)
	key, _, err := authSvc.Create(context.Background(), tenant.ID, "webhooks", scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := webhook.NewSecretBox(bytes.Repeat([]byte{3}, 32))
	service, _ := webhook.NewService(db, box, webhook.URLPolicy{AllowHTTP: true, AllowPrivate: true})
	h := newEmailHandler(db, mustStore(t))
	mux := newMux(h, authSvc, func() error { return nil }, routeServices{webhooks: service})
	return authInjector{next: mux, token: key.Raw}, db, tenant, service
}

func TestWebhookSecretVisibleOnlyOnCreateAndRotation(t *testing.T) {
	mux, _, _, _ := setupWebhookAPI(t, []string{string(auth.ScopeWebhooksRead), string(auth.ScopeWebhooksWrite)})
	created := doJSON(t, mux, "POST", "/v1/webhooks", map[string]any{
		"url": "http://127.0.0.1:9876/hook", "events": []string{"email.delivered", "email.failed"},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create got %d: %s", created.Code, created.Body.String())
	}
	var resource webhookResource
	if err := json.Unmarshal(created.Body.Bytes(), &resource); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resource.SigningSecret, "whsec_") {
		t.Fatal("create did not return one-time signing secret")
	}
	firstSecret := resource.SigningSecret
	for _, path := range []string{"/v1/webhooks/" + resource.ID, "/v1/webhooks"} {
		rec := doJSON(t, mux, "GET", path, nil)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "whsec_") || strings.Contains(rec.Body.String(), "secret_ciphertext") {
			t.Fatalf("secret exposed by %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	rotated := doJSON(t, mux, "POST", "/v1/webhooks/"+resource.ID+"/rotate-secret", nil)
	if rotated.Code != http.StatusOK {
		t.Fatalf("rotate got %d: %s", rotated.Code, rotated.Body.String())
	}
	var replacement webhookResource
	_ = json.Unmarshal(rotated.Body.Bytes(), &replacement)
	if replacement.SigningSecret == "" || replacement.SigningSecret == firstSecret {
		t.Fatal("rotation did not return a new one-time secret")
	}
}

func TestWebhookScopesAndTenantIsolation(t *testing.T) {
	writeOnly, db, tenantA, service := setupWebhookAPI(t, []string{string(auth.ScopeWebhooksWrite)})
	created := doJSON(t, writeOnly, "POST", "/v1/webhooks", map[string]any{"url": "http://127.0.0.1/h", "events": []string{"email.failed"}})
	if created.Code != http.StatusCreated {
		t.Fatalf("write scope rejected: %s", created.Body.String())
	}
	var own webhookResource
	_ = json.Unmarshal(created.Body.Bytes(), &own)
	if rec := doJSON(t, writeOnly, "GET", "/v1/webhooks", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("write-only key read = %d", rec.Code)
	}
	readOnly, _, _, _ := setupWebhookAPI(t, []string{string(auth.ScopeWebhooksRead)})
	if rec := doJSON(t, readOnly, "POST", "/v1/webhooks", map[string]any{"url": "http://127.0.0.1/h", "events": []string{"email.failed"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("read-only key wrote = %d", rec.Code)
	}
	neither, _, _, _ := setupWebhookAPI(t, []string{string(auth.ScopeEmailsRead)})
	if rec := doJSON(t, neither, "GET", "/v1/webhooks", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("key without webhook scope read = %d", rec.Code)
	}

	tenantB := newTestTenant(t, db)
	authSvc := auth.NewService(db, nil)
	keyB, _, _ := authSvc.Create(context.Background(), tenantB.ID, "b", []string{string(auth.ScopeWebhooksRead), string(auth.ScopeWebhooksWrite)}, nil)
	h := newEmailHandler(db, mustStore(t))
	muxB := authInjector{next: newMux(h, authSvc, func() error { return nil }, routeServices{webhooks: service}), token: keyB.Raw}
	for _, methodPath := range [][2]string{
		{"GET", "/v1/webhooks/" + own.ID},
		{"DELETE", "/v1/webhooks/" + own.ID},
		{"POST", "/v1/webhooks/" + own.ID + "/rotate-secret"},
		{"GET", "/v1/webhooks/" + own.ID + "/deliveries"},
	} {
		if rec := doJSON(t, muxB, methodPath[0], methodPath[1], nil); rec.Code != http.StatusNotFound {
			t.Fatalf("tenant B %s %s = %d", methodPath[0], methodPath[1], rec.Code)
		}
	}
	if tenantA.ID == tenantB.ID {
		t.Fatal("invalid tenant fixture")
	}
}

func TestWebhookValidationAndDelete(t *testing.T) {
	mux, _, _, _ := setupWebhookAPI(t, []string{string(auth.ScopeWebhooksRead), string(auth.ScopeWebhooksWrite)})
	for _, body := range []map[string]any{
		{"url": "ftp://example.com/h", "events": []string{"email.failed"}},
		{"url": "http://127.0.0.1/h", "events": []string{"email.typo"}},
		{"url": "http://127.0.0.1/h", "events": []string{}},
	} {
		if rec := doJSON(t, mux, "POST", "/v1/webhooks", body); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("invalid webhook accepted: %d %s", rec.Code, rec.Body.String())
		}
	}
	created := doJSON(t, mux, "POST", "/v1/webhooks", map[string]any{"url": "http://127.0.0.1/h", "events": []string{"email.failed"}})
	var resource webhookResource
	_ = json.Unmarshal(created.Body.Bytes(), &resource)
	if rec := doJSON(t, mux, "DELETE", "/v1/webhooks/"+resource.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := doJSON(t, mux, "GET", "/v1/webhooks/"+resource.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted webhook remains visible: %d", rec.Code)
	}
}

type dnsFailResolver struct{}

func (dnsFailResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, &net.DNSError{Err: "read udp 10.9.8.7:53: i/o timeout", Server: "10.9.8.7:53"}
}

func TestCreateWebhookDNSFailureIsTemporaryAndDoesNotLeakResolverDetail(t *testing.T) {
	_, db, _, _, raw := setupMuxNoAuth(t)
	box, _ := webhook.NewSecretBox(make([]byte, 32))
	svc, _ := webhook.NewService(db, box, webhook.URLPolicy{Resolver: dnsFailResolver{}})
	router := newMux(newEmailHandler(db, nil), auth.NewService(db, nil), func() error { return nil }, routeServices{webhooks: svc})
	req := httptest.NewRequest("POST", "/v1/webhooks", strings.NewReader(`{"url":"https://hooks.example.com/x","events":["email.queued"]}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "10.9.8.7") || strings.Contains(rec.Body.String(), "timeout") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}
