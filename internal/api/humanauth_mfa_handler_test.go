package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/humanauth"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

// MFA verify has its own tight per-IP bucket: after MFAVerifyIPBurst wrong
// attempts the next is 429, and OAuth for an unconfigured provider is a clear 404.
func TestMFAVerifyRateLimitedAndOAuthNotConfigured(t *testing.T) {
	rig := newAbuseRig(t, testPolicy())
	svc, err := humanauth.NewService(rig.db, []byte("test-secret-at-least-32-bytes-long!!"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := testPolicy()
	ac := &AbuseControls{Limiter: rig.limit, Policy: p}
	mux := newMux(newEmailHandler(rig.db, store), auth.NewService(rig.db, nil), func() error { return nil }, routeServices{abuse: ac, humanAuth: svc})

	do := func(method, path, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.9:1234"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}
	for i := 0; i < p.MFAVerifyIPBurst; i++ {
		if c := do("POST", "/v1/auth/mfa/verify", `{"mfa_token":"nope","code":"123456"}`); c != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d want 401", i, c)
		}
	}
	if c := do("POST", "/v1/auth/mfa/verify", `{"mfa_token":"nope","code":"123456"}`); c != http.StatusTooManyRequests {
		t.Fatalf("over burst: got %d want 429", c)
	}
	if c := do("GET", "/v1/auth/oauth/google/start", ""); c != http.StatusNotFound {
		t.Fatalf("unconfigured provider: got %d want 404", c)
	}
	// Enroll requires a session.
	if c := do("POST", "/v1/auth/mfa/enroll", `{}`); c != http.StatusUnauthorized {
		t.Fatalf("enroll without session: got %d want 401", c)
	}
}
