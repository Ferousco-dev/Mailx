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

// TestOAuthCallbackRejectsWrongReferer is a defense-in-depth check for
// RSK-046 (login CSRF): a callback request whose Referer names neither the
// provider nor nothing at all (i.e. clearly not a redirect from the
// provider's own consent page) is rejected before the state is even looked
// up, while a request with no Referer at all (the common legitimate case;
// browsers/extensions may omit it) is allowed through to the normal
// invalid-state handling.
func TestOAuthCallbackRejectsWrongReferer(t *testing.T) {
	rig := newAbuseRig(t, testPolicy())
	svc, err := humanauth.NewService(rig.db, []byte("test-secret-at-least-32-bytes-long!!"),
		humanauth.WithOAuthProvider(humanauth.GoogleProvider("cid", "shh", "https://api.mailx.test")))
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ac := &AbuseControls{Limiter: rig.limit, Policy: testPolicy()}
	mux := newMux(newEmailHandler(rig.db, store), auth.NewService(rig.db, nil), func() error { return nil }, routeServices{abuse: ac, humanAuth: svc})

	do := func(referer string) int {
		req := httptest.NewRequest("GET", "/v1/auth/oauth/google/callback?code=x&state=y", nil)
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		req.RemoteAddr = "203.0.113.10:1234"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}
	if c := do("https://attacker.example/deliver-my-oauth-link"); c != http.StatusUnauthorized {
		t.Fatalf("wrong referer: got %d want 401", c)
	}
	if c := do("https://accounts.google.com/o/oauth2/v2/auth"); c != http.StatusUnauthorized {
		// Reaches the normal (unknown state) path, not the referer check.
		t.Fatalf("correct provider referer: got %d want 401 (invalid state)", c)
	}
	if c := do(""); c != http.StatusUnauthorized {
		// No referer at all is allowed through to the same normal path.
		t.Fatalf("no referer: got %d want 401 (invalid state)", c)
	}
}
