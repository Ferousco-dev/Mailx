package humanauth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func tokenFromText(t *testing.T, text string) string {
	t.Helper()
	idx := strings.Index(text, "token=")
	if idx == -1 {
		t.Fatalf("no token in email body: %q", text)
	}
	raw := text[idx+len("token="):]
	if end := strings.IndexAny(raw, "\n "); end != -1 {
		raw = raw[:end]
	}
	return raw
}

func (m *fakeMailer) verifySnapshot() []struct{ to, subject, text, html string } {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]struct{ to, subject, text, html string }(nil), m.verifyCalls...)
}

func TestSignUpSendsVerificationAndVerifyEmail(t *testing.T) {
	db := newTestDB(t)
	mailer := &fakeMailer{}
	svc, err := NewService(db, testSecret(), WithMailer(mailer), WithDashboardBaseURL("https://app.mailx.dev"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sess, err := svc.SignUp(ctx, "Ada", "ada@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Human.EmailVerifiedAt != nil {
		t.Fatal("new account must start unverified")
	}
	calls := mailer.verifySnapshot()
	if len(calls) != 1 || calls[0].to != "ada@example.com" {
		t.Fatalf("expected one verification email to ada, got %+v", calls)
	}
	if !strings.Contains(calls[0].text, "Hello Ada, welcome to MailX") ||
		!strings.Contains(calls[0].text, "https://app.mailx.dev/verify-email?token=") {
		t.Fatalf("unexpected verification body: %q", calls[0].text)
	}
	raw := tokenFromText(t, calls[0].text)

	if err := svc.VerifyEmail(ctx, raw); err != nil {
		t.Fatal(err)
	}
	h, err := db.GetHuman(ctx, sess.Human.ID)
	if err != nil {
		t.Fatal(err)
	}
	if h.EmailVerifiedAt == nil {
		t.Fatal("expected email_verified_at to be set")
	}
	if err := svc.VerifyEmail(ctx, raw); !errors.Is(err, ErrEmailVerificationTokenInvalid) {
		t.Fatalf("reuse: expected ErrEmailVerificationTokenInvalid, got %v", err)
	}
	if err := svc.VerifyEmail(ctx, "not-a-real-token"); !errors.Is(err, ErrEmailVerificationTokenInvalid) {
		t.Fatalf("unknown: expected ErrEmailVerificationTokenInvalid, got %v", err)
	}
	// Resend to an already-verified account is a silent no-op.
	if err := svc.ResendVerification(ctx, "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	if n := len(mailer.verifySnapshot()); n != 1 {
		t.Fatalf("resend to verified account sent mail (%d total)", n)
	}
}

func TestSignUpSucceedsWhenVerificationEmailFails(t *testing.T) {
	db := newTestDB(t)
	mailer := &fakeMailer{failNext: true}
	svc, err := NewService(db, testSecret(), WithMailer(mailer), WithDashboardBaseURL("https://app.mailx.dev"))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := svc.SignUp(context.Background(), "Ada", "ada@example.com", "hunter22hunter")
	if err != nil || sess.AccessToken == "" {
		t.Fatalf("signup must succeed despite mail failure: %v", err)
	}
	// No mailer at all: same.
	svc2, _ := NewService(db, testSecret())
	if _, err := svc2.SignUp(context.Background(), "Bo", "bo@example.com", "hunter22hunter"); err != nil {
		t.Fatalf("signup without mailer: %v", err)
	}
}

func TestVerifyEmailExpiredToken(t *testing.T) {
	db := newTestDB(t)
	mailer := &fakeMailer{}
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	current := start
	svc, err := NewService(db, testSecret(), WithMailer(mailer), WithDashboardBaseURL("https://app.mailx.dev"),
		WithNow(func() time.Time { return current }))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := svc.SignUp(ctx, "Ada", "ada@example.com", "hunter22hunter"); err != nil {
		t.Fatal(err)
	}
	raw := tokenFromText(t, mailer.verifySnapshot()[0].text)
	current = start.Add(EmailVerificationTokenTTL + time.Second)
	if err := svc.VerifyEmail(ctx, raw); !errors.Is(err, ErrEmailVerificationTokenInvalid) {
		t.Fatalf("expected ErrEmailVerificationTokenInvalid for expired token, got %v", err)
	}
}

func TestResendVerificationSupersedesOldToken(t *testing.T) {
	db := newTestDB(t)
	mailer := &fakeMailer{}
	svc, err := NewService(db, testSecret(), WithMailer(mailer), WithDashboardBaseURL("https://app.mailx.dev"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := svc.SignUp(ctx, "Ada", "ada@example.com", "hunter22hunter"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ResendVerification(ctx, "unknown@example.com"); err != nil {
		t.Fatal(err)
	}
	if n := len(mailer.verifySnapshot()); n != 1 {
		t.Fatalf("unknown email must not send, got %d emails", n)
	}
	if err := svc.ResendVerification(ctx, "ADA@example.com"); err != nil {
		t.Fatal(err)
	}
	calls := mailer.verifySnapshot()
	if len(calls) != 2 {
		t.Fatalf("expected a second verification email, got %d", len(calls))
	}
	oldTok, newTok := tokenFromText(t, calls[0].text), tokenFromText(t, calls[1].text)
	if err := svc.VerifyEmail(ctx, oldTok); !errors.Is(err, ErrEmailVerificationTokenInvalid) {
		t.Fatalf("old token must be superseded, got %v", err)
	}
	if err := svc.VerifyEmail(ctx, newTok); err != nil {
		t.Fatalf("new token must work: %v", err)
	}
}

// TestConcurrentResendsLeaveExactlyOneValidToken is the DEC-226 lesson
// applied here: concurrent invalidate-then-insert must never leave zero
// (both superseded) or two valid tokens. IssueEmailVerificationToken
// serializes on the humans row (FOR UPDATE), so exactly one - the last
// committed - survives.
func TestConcurrentResendsLeaveExactlyOneValidToken(t *testing.T) {
	db := newTestDB(t)
	mailer := &fakeMailer{}
	svc, err := NewService(db, testSecret(), WithMailer(mailer), WithDashboardBaseURL("https://app.mailx.dev"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := svc.SignUp(ctx, "Ada", "ada@example.com", "hunter22hunter"); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 10; round++ {
		const n = 4
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if err := svc.ResendVerification(ctx, "ada@example.com"); err != nil {
					t.Errorf("resend: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		var valid int
		for _, c := range mailer.verifySnapshot() {
			rec, err := db.GetEmailVerificationTokenByHash(ctx, hashRawToken(tokenFromText(t, c.text)))
			if err != nil {
				t.Fatal(err)
			}
			if rec.UsedAt == nil {
				valid++
			}
		}
		if valid != 1 {
			t.Fatalf("round %d: expected exactly one valid token, got %d", round, valid)
		}
	}
}

// TestFailedResendDoesNotInvalidateTheWorkingVerificationLink is a
// regression test for a bug caught before merge (same class as DEC-218): a
// resend whose email delivery fails must NOT invalidate the previous,
// still-working verification link - otherwise a transient mailer failure
// leaves the account with neither a delivered new link nor a working old
// one.
func TestFailedResendDoesNotInvalidateTheWorkingVerificationLink(t *testing.T) {
	db := newTestDB(t)
	mailer := &fakeMailer{}
	svc, err := NewService(db, testSecret(), WithMailer(mailer), WithDashboardBaseURL("https://app.mailx.dev"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := svc.SignUp(ctx, "Ada", "ada@example.com", "hunter22hunter"); err != nil {
		t.Fatal(err)
	}
	firstToken := tokenFromText(t, mailer.verifySnapshot()[0].text)

	mailer.failNext = true
	if err := svc.ResendVerification(ctx, "ada@example.com"); err == nil {
		t.Fatal("expected the simulated send failure to surface as an error")
	}

	// The original verification link must still work: the failed resend's
	// delivery failure must not have invalidated it.
	if err := svc.VerifyEmail(ctx, firstToken); err != nil {
		t.Fatalf("expected the original verification token to still be valid after a failed resend: %v", err)
	}
}
