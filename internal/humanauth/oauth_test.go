package humanauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// fakeGoogle is an httptest TLS server standing in for Google's token and
// userinfo endpoints (mirrors internal/billing's fake-Paystack approach).
type fakeGoogle struct {
	srv           *httptest.Server
	sub, email    string
	emailVerified bool
	tokenStatus   int
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	f := &fakeGoogle{sub: "g-123", email: "Grace@Example.com", emailVerified: true, tokenStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != "good-code" || r.Form.Get("client_secret") != "shh" || r.Form.Get("grant_type") != "authorization_code" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(f.tokenStatus)
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at-1", "token_type": "Bearer"})
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sub": f.sub, "email": f.email, "email_verified": f.emailVerified, "name": "Grace Hopper", "picture": "https://img.example/g.png"})
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGoogle) provider() *OAuthProvider {
	p := GoogleProvider("cid", "shh", "https://api.mailx.test")
	p.AuthURL, p.TokenURL, p.UserInfoURL = f.srv.URL+"/auth", f.srv.URL+"/token", f.srv.URL+"/userinfo"
	p.HTTPClient = f.srv.Client()
	return p
}

func startState(t *testing.T, svc *Service) string {
	t.Helper()
	u, err := svc.StartOAuth(context.Background(), "google")
	if err != nil {
		t.Fatal(err)
	}
	pu, _ := url.Parse(u)
	q := pu.Query()
	if q.Get("redirect_uri") != "https://api.mailx.test/v1/auth/oauth/google/callback" || q.Get("client_id") != "cid" || q.Get("state") == "" {
		t.Fatalf("bad authorization url %s", u)
	}
	return q.Get("state")
}

func TestOAuthNewAccountLinkAndState(t *testing.T) {
	fg := newFakeGoogle(t)
	db := newTestDB(t)
	svc, err := NewService(db, testSecret(), WithOAuthProvider(fg.provider()))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// New account created.
	s1, err := svc.CompleteOAuth(ctx, "google", "good-code", startState(t, svc))
	if err != nil || s1.AccessToken == "" || s1.Human.Email != "Grace@Example.com" {
		t.Fatalf("new account: %v %+v", err, s1.Human)
	}
	// OAuth-created account has no usable password.
	if _, err := svc.Login(ctx, "grace@example.com", "!oauth-no-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("oauth account password login: %v", err)
	}
	// Same identity again -> same human.
	s2, err := svc.CompleteOAuth(ctx, "google", "good-code", startState(t, svc))
	if err != nil || s2.Human.ID != s1.Human.ID {
		t.Fatalf("repeat login: %v", err)
	}

	// Existing password account is LINKED (case-insensitive), not duplicated.
	pw, err := svc.SignUp(ctx, "Alan", "alan@example.com", "password123")
	if err != nil {
		t.Fatal(err)
	}
	fg.sub, fg.email = "g-456", "ALAN@example.com"
	s3, err := svc.CompleteOAuth(ctx, "google", "good-code", startState(t, svc))
	if err != nil || s3.Human.ID != pw.Human.ID {
		t.Fatalf("link: %v got %s want %s", err, s3.Human.ID, pw.Human.ID)
	}

	// State: missing, unknown, reused, wrong provider.
	if _, err := svc.CompleteOAuth(ctx, "google", "good-code", ""); !errors.Is(err, ErrOAuthStateInvalid) {
		t.Fatalf("missing state: %v", err)
	}
	if _, err := svc.CompleteOAuth(ctx, "google", "good-code", "deadbeef"); !errors.Is(err, ErrOAuthStateInvalid) {
		t.Fatalf("unknown state: %v", err)
	}
	st := startState(t, svc)
	if _, err := svc.CompleteOAuth(ctx, "google", "good-code", st); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteOAuth(ctx, "google", "good-code", st); !errors.Is(err, ErrOAuthStateInvalid) {
		t.Fatalf("reused state: %v", err)
	}
	// Expired state.
	st = startState(t, svc)
	svc.now = func() time.Time { return time.Now().UTC().Add(OAuthStateTTL + time.Minute) }
	if _, err := svc.CompleteOAuth(ctx, "google", "good-code", st); !errors.Is(err, ErrOAuthStateInvalid) {
		t.Fatalf("expired state: %v", err)
	}
}

func TestOAuthProviderErrorsAndNotConfigured(t *testing.T) {
	fg := newFakeGoogle(t)
	svc, err := NewService(newTestDB(t), testSecret(), WithOAuthProvider(fg.provider()))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := svc.CompleteOAuth(ctx, "google", "bad-code", startState(t, svc)); !errors.Is(err, ErrOAuthProvider) {
		t.Fatalf("bad code: %v", err)
	}
	if _, err := svc.CompleteOAuth(ctx, "google", "", startState(t, svc)); !errors.Is(err, ErrOAuthProvider) {
		t.Fatalf("denied consent: %v", err)
	}
	fg.tokenStatus = http.StatusInternalServerError
	if _, err := svc.CompleteOAuth(ctx, "google", "good-code", startState(t, svc)); !errors.Is(err, ErrOAuthProvider) {
		t.Fatalf("provider 500: %v", err)
	}
	fg.tokenStatus, fg.emailVerified = http.StatusOK, false
	if _, err := svc.CompleteOAuth(ctx, "google", "good-code", startState(t, svc)); !errors.Is(err, ErrOAuthProvider) {
		t.Fatalf("unverified email: %v", err)
	}
	if _, err := svc.StartOAuth(ctx, "github"); !errors.Is(err, ErrOAuthProviderNotConfigured) {
		t.Fatalf("github not configured: %v", err)
	}
	if _, err := svc.CompleteOAuth(ctx, "github", "x", "y"); !errors.Is(err, ErrOAuthProviderNotConfigured) {
		t.Fatalf("github callback not configured: %v", err)
	}
	// Non-https endpoints are rejected at construction.
	p := fg.provider()
	p.TokenURL = "http://evil.example/token"
	if _, err := NewService(newTestDB(t), testSecret(), WithOAuthProvider(p)); err == nil {
		t.Fatal("http token endpoint accepted")
	}
}

// OAuth must never bypass MFA on an MFA-enabled (linked) account.
func TestOAuthRespectsMFA(t *testing.T) {
	f := newMFAService(t)
	fg := newFakeGoogle(t)
	WithOAuthProvider(fg.provider())(f.svc)
	ctx := context.Background()
	sess, err := f.svc.SignUp(ctx, "Grace", "grace@example.com", "password123")
	if err != nil {
		t.Fatal(err)
	}
	enr, err := f.svc.EnrollMFA(ctx, sess.Human.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ConfirmMFA(ctx, sess.Human.ID, codeAt(t, enr.Secret, *f.clock)); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.CompleteOAuth(ctx, "google", "good-code", startState(t, f.svc))
	var mfa *MFARequiredError
	if !errors.As(err, &mfa) {
		t.Fatalf("oauth on MFA account returned %v, want MFARequiredError", err)
	}
}
