package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

func rawRequest(t *testing.T, mux http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Add(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not a valid error envelope: %v (%s)", err, rec.Body.String())
	}
	return body
}

func TestAuthMissingHeader(t *testing.T) {
	mux, _, _, _, _ := setupMuxNoAuth(t)
	rec := rawRequest(t, mux, "GET", "/v1/emails", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Error.Type != ErrAuthentication {
		t.Fatalf("unexpected error type: %+v", body)
	}
}

func TestAuthMalformedHeaders(t *testing.T) {
	mux, _, _, _, rawKey := setupMuxNoAuth(t)
	cases := map[string]string{
		"empty":            "",
		"wrong scheme":     "Basic dXNlcjpwYXNz",
		"bearer no token":  "Bearer",
		"bearer empty":     "Bearer ",
		"lowercase bearer": "bearer " + rawKey,
		"truncated":        "Bearer mx_abc",
		"random garbage":   "Bearer " + strings.Repeat("z", 100),
		"trailing junk":    "Bearer " + rawKey + " extra",
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			rec := rawRequest(t, mux, "GET", "/v1/emails", map[string]string{"Authorization": header})
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: got %d: %s", name, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAuthMultipleAuthorizationHeaders(t *testing.T) {
	mux, _, _, _, rawKey := setupMuxNoAuth(t)
	req := httptest.NewRequest("GET", "/v1/emails", nil)
	req.Header.Add("Authorization", "Bearer "+rawKey)
	req.Header.Add("Authorization", "Bearer "+rawKey)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected multiple Authorization headers to be rejected, got %d", rec.Code)
	}
}

func TestAuthOversizedCredential(t *testing.T) {
	mux, _, _, _, _ := setupMuxNoAuth(t)
	huge := "Bearer " + strings.Repeat("a", 10000)
	rec := rawRequest(t, mux, "GET", "/v1/emails", map[string]string{"Authorization": huge})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthUnknownKeyAndWrongSecret(t *testing.T) {
	mux, _, _, _, rawKey := setupMuxNoAuth(t)
	unknown := "mx_0000000000000000000000000000ff_" + strings.Repeat("0", 64)
	if rec := rawRequest(t, mux, "GET", "/v1/emails", map[string]string{"Authorization": "Bearer " + unknown}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key: got %d", rec.Code)
	}
	tampered := rawKey[:len(rawKey)-1] + "0"
	if tampered == rawKey {
		tampered = rawKey[:len(rawKey)-1] + "1"
	}
	if rec := rawRequest(t, mux, "GET", "/v1/emails", map[string]string{"Authorization": "Bearer " + tampered}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: got %d", rec.Code)
	}
}

func TestAuthRevokedKeyRejected(t *testing.T) {
	_, db, tenant, authSvc, rawKey := setupMuxNoAuth(t)
	h := newEmailHandler(db, mustStore(t))
	mux := newMux(h, authSvc, func() error { return nil })

	keys, err := authSvc.List(context.Background(), tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := authSvc.Revoke(context.Background(), keys[0].KeyID); err != nil {
		t.Fatal(err)
	}
	rec := rawRequest(t, mux, "GET", "/v1/emails", map[string]string{"Authorization": "Bearer " + rawKey})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAuthExpiredKeyRejected proves the boundary end-to-end through the
// HTTP layer using an already-past TTL; the precise before/at/after-expiry
// clock-boundary behavior is covered at the auth package level (see
// internal/auth/service_test.go's TestServiceAuthenticateExpirationBoundary),
// since Service's clock is only mockable within that package.
func TestAuthExpiredKeyRejected(t *testing.T) {
	db := newTestDB(t)
	tenant := newTestTenant(t, db)
	authSvc := auth.NewService(db, nil)
	pastTTL := -time.Hour
	gen, _, err := authSvc.Create(context.Background(), tenant.ID, "y", []string{string(auth.ScopeEmailsRead)}, &pastTTL)
	if err != nil {
		t.Fatal(err)
	}
	h := newEmailHandler(db, mustStore(t))
	mux := newMux(h, authSvc, func() error { return nil })
	rec := rawRequest(t, mux, "GET", "/v1/emails", map[string]string{"Authorization": "Bearer " + gen.Raw})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthValidKeyWorks(t *testing.T) {
	mux, _, _, _, rawKey := setupMuxNoAuth(t)
	rec := rawRequest(t, mux, "GET", "/v1/emails", map[string]string{"Authorization": "Bearer " + rawKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

// ------------------------------------------------------ authorization --

func TestAuthorizationSendOnlyScope(t *testing.T) {
	db := newTestDB(t)
	tenant := newTestTenant(t, db)
	authSvc := auth.NewService(db, nil)
	gen, _, err := authSvc.Create(context.Background(), tenant.ID, "send-only", []string{string(auth.ScopeEmailsSend)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newEmailHandler(db, mustStore(t))
	mux := newMux(h, authSvc, func() error { return nil })
	authed := authInjector{next: mux, token: gen.Raw}

	sendRec := doJSON(t, authed, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com"}, "text": "x",
	})
	if sendRec.Code != http.StatusAccepted {
		t.Fatalf("send-only key should be able to POST: got %d: %s", sendRec.Code, sendRec.Body.String())
	}
	if rec := rawRequest(t, authed, "GET", "/v1/emails", map[string]string{"Authorization": "Bearer " + gen.Raw}); rec.Code != http.StatusForbidden {
		t.Fatalf("send-only key must not be able to LIST: got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthorizationReadOnlyScope(t *testing.T) {
	db := newTestDB(t)
	tenant := newTestTenant(t, db)
	authSvc := auth.NewService(db, nil)
	gen, _, err := authSvc.Create(context.Background(), tenant.ID, "read-only", []string{string(auth.ScopeEmailsRead)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newEmailHandler(db, mustStore(t))
	mux := newMux(h, authSvc, func() error { return nil })
	authed := authInjector{next: mux, token: gen.Raw}

	if rec := doJSON(t, authed, "GET", "/v1/emails", nil); rec.Code != http.StatusOK {
		t.Fatalf("read-only key should be able to LIST: got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, authed, "GET", "/v1/emails/nonexistent", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("read-only key should be able to attempt GET (404 for a missing id): got %d: %s", rec.Code, rec.Body.String())
	}
	sendRec := doJSON(t, authed, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com"}, "text": "x",
	})
	if sendRec.Code != http.StatusForbidden {
		t.Fatalf("read-only key must not be able to POST: got %d: %s", sendRec.Code, sendRec.Body.String())
	}
}

func TestAuthorizationBothScopesAllowEverything(t *testing.T) {
	mux, _, _ := setupMux(t) // setupMux already grants both scopes
	sendRec := doJSON(t, mux, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com"}, "text": "x",
	})
	if sendRec.Code != http.StatusAccepted {
		t.Fatalf("got %d", sendRec.Code)
	}
	if rec := doJSON(t, mux, "GET", "/v1/emails", nil); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestCreateAPIKeyRejectsUnknownScope(t *testing.T) {
	db := newTestDB(t)
	tenant := newTestTenant(t, db)
	authSvc := auth.NewService(db, nil)
	if _, _, err := authSvc.Create(context.Background(), tenant.ID, "x", []string{"emails:delete"}, nil); err == nil {
		t.Fatal("expected an unknown scope to be rejected at creation")
	}
}

// -------------------------------------------------- database failure ---

type unavailableAuth struct{}

func (unavailableAuth) Authenticate(ctx context.Context, raw string) (database.APIKey, error) {
	return database.APIKey{}, auth.ErrUnavailable
}

// TestAuthDatabaseUnavailableFailsClosed proves the fail-closed contract:
// when authentication cannot reach its source of truth, the request must
// be rejected (503), never silently treated as authenticated.
func TestAuthDatabaseUnavailableFailsClosed(t *testing.T) {
	h := newEmailHandler(nil, mustStore(t))
	mux := newMux(h, unavailableAuth{}, func() error { return nil })
	rec := rawRequest(t, mux, "GET", "/v1/emails", map[string]string{"Authorization": "Bearer mx_" + strings.Repeat("a", 32) + "_" + strings.Repeat("b", 64)})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 fail-closed, got %d: %s", rec.Code, rec.Body.String())
	}
}

func mustStore(t *testing.T) *storage.FileStore {
	t.Helper()
	s, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}
