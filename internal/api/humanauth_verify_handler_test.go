package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/humanauth"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

// Resend-verification: identical 200 body for unknown vs existing
// accounts (anti-enumeration), then 429 past its own per-IP burst.
// Verify-email: unknown token is the generic 422.
func TestEmailVerificationHandlersAndResendRateLimit(t *testing.T) {
	rig := newAbuseRig(t, testPolicy())
	svc, err := humanauth.NewService(rig.db, []byte("test-secret-at-least-32-bytes-long!!"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SignUp(t.Context(), "Ada", "ada@example.com", "hunter22hunter"); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := testPolicy()
	ac := &AbuseControls{Limiter: rig.limit, Policy: p}
	mux := newMux(newEmailHandler(rig.db, store), auth.NewService(rig.db, nil), func() error { return nil }, routeServices{abuse: ac, humanAuth: svc})

	// Unique IP per run: the Redis limiter state outlives a single test run.
	ip := fmt.Sprintf("198.51.100.%d:1234", time.Now().UnixMicro()%250+1)
	do := func(path, body string) (int, string) {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = ip
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	if c, body := do("/v1/auth/verify-email", `{"token":"not-a-real-token"}`); c != http.StatusUnprocessableEntity || !strings.Contains(body, "invalid_verification_token") {
		t.Fatalf("unknown token: got %d %s", c, body)
	}

	c1, b1 := do("/v1/auth/resend-verification", `{"email":"ada@example.com"}`)
	c2, b2 := do("/v1/auth/resend-verification", `{"email":"nobody@example.com"}`)
	if c1 != http.StatusOK || c2 != http.StatusOK || b1 != b2 {
		t.Fatalf("resend must be generic: %d %q vs %d %q", c1, b1, c2, b2)
	}
	for i := 2; i < p.EmailVerificationResendIPBurst; i++ {
		if c, _ := do("/v1/auth/resend-verification", `{"email":"nobody@example.com"}`); c != http.StatusOK {
			t.Fatalf("attempt %d: got %d want 200", i, c)
		}
	}
	if c, body := do("/v1/auth/resend-verification", `{"email":"nobody@example.com"}`); c != http.StatusTooManyRequests || !strings.Contains(body, "email_verification_resend_rate_limited") {
		t.Fatalf("over burst: got %d %s want 429", c, body)
	}
}
