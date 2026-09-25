package humanauth

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/secretbox"
)

// RFC 6238 Appendix B SHA-1 vectors (8 digits; the 6-digit value is the
// low-order 6 digits because both are code mod 10^n).
func TestTOTPRFC6238Vectors(t *testing.T) {
	key := []byte("12345678901234567890")
	for _, v := range []struct {
		unix int64
		want string
	}{
		{59, "94287082"}, {1111111109, "07081804"}, {1111111111, "14050471"},
		{1234567890, "89005924"}, {2000000000, "69279037"}, {20000000000, "65353130"},
	} {
		step := uint64(v.unix / 30)
		if got := hotp(key, step, 8); got != v.want {
			t.Errorf("T=%d: got %s want %s", v.unix, got, v.want)
		}
		if got := hotp(key, step, 6); got != v.want[2:] {
			t.Errorf("T=%d 6-digit: got %s want %s", v.unix, got, v.want[2:])
		}
	}
	now := time.Unix(1111111111, 0)
	for _, d := range []int64{-30, 0, 30} {
		if _, ok := verifyTOTP(key, hotp(key, uint64((now.Unix()+d)/30), 6), now); !ok {
			t.Errorf("skew %d rejected", d)
		}
	}
	if _, ok := verifyTOTP(key, hotp(key, uint64((now.Unix()+90)/30), 6), now); ok {
		t.Error("code 3 steps away accepted")
	}
}

type mfaFixture struct {
	svc   *Service
	clock *time.Time
	mu    *sync.Mutex
}

func (f mfaFixture) advance(d time.Duration) { f.mu.Lock(); *f.clock = f.clock.Add(d); f.mu.Unlock() }

func newMFAService(t *testing.T) mfaFixture {
	t.Helper()
	db := newTestDB(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now().UTC().Truncate(time.Second)
	mu := &sync.Mutex{}
	svc, err := NewService(db, testSecret(), WithMFABox(box), WithNow(func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }))
	if err != nil {
		t.Fatal(err)
	}
	return mfaFixture{svc: svc, clock: &clock, mu: mu}
}

func codeAt(t *testing.T, secret string, now time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	return hotp(key, uint64(totpStep(now)), 6)
}

func loginChallenge(t *testing.T, svc *Service, email string) string {
	t.Helper()
	_, err := svc.Login(context.Background(), email, "password123")
	var mfa *MFARequiredError
	if !errors.As(err, &mfa) || mfa.ChallengeToken == "" {
		t.Fatalf("expected MFA challenge, got %v", err)
	}
	return mfa.ChallengeToken
}

func TestMFAFullFlow(t *testing.T) {
	f := newMFAService(t)
	svc, ctx := f.svc, context.Background()
	sess, err := svc.SignUp(ctx, "Ada", "ada@example.com", "password123")
	if err != nil {
		t.Fatal(err)
	}
	id := sess.Human.ID

	enr, err := svc.EnrollMFA(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(enr.BackupCodes) != 10 || !strings.HasPrefix(enr.OTPAuthURI, "otpauth://totp/MailX:") || !strings.Contains(enr.OTPAuthURI, "secret="+enr.Secret) {
		t.Fatalf("bad enrollment: %+v", enr)
	}
	// Not active before confirm: login still yields a full session.
	if _, err := svc.Login(ctx, "ada@example.com", "password123"); err != nil {
		t.Fatalf("login before confirm: %v", err)
	}
	if err := svc.ConfirmMFA(ctx, id, "000000"); !errors.Is(err, ErrMFACodeInvalid) && codeAt(t, enr.Secret, *f.clock) != "000000" {
		t.Fatalf("wrong confirm code: %v", err)
	}
	if err := svc.ConfirmMFA(ctx, id, codeAt(t, enr.Secret, *f.clock)); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, err := svc.EnrollMFA(ctx, id); !errors.Is(err, ErrMFAAlreadyEnabled) {
		t.Fatalf("re-enroll while enabled: %v", err)
	}

	// Login now requires MFA; the challenge is not an access token.
	ch := loginChallenge(t, svc, "ada@example.com")
	if _, err := svc.VerifyAccessToken(ch); err == nil {
		t.Fatal("challenge token verified as an access token")
	}
	// The confirming code's step is burned: advance one step first.
	f.advance(30 * time.Second)
	s2, err := svc.VerifyMFA(ctx, ch, codeAt(t, enr.Secret, *f.clock))
	if err != nil || s2.AccessToken == "" || s2.RefreshToken == "" {
		t.Fatalf("verify: %v", err)
	}
	// Challenge cannot be reused after success.
	if _, err := svc.VerifyMFA(ctx, ch, codeAt(t, enr.Secret, *f.clock)); !errors.Is(err, ErrMFAChallengeInvalid) {
		t.Fatalf("reused challenge: %v", err)
	}
	// Same TOTP code cannot be replayed on a fresh challenge.
	ch = loginChallenge(t, svc, "ada@example.com")
	if _, err := svc.VerifyMFA(ctx, ch, codeAt(t, enr.Secret, *f.clock)); !errors.Is(err, ErrMFAChallengeInvalid) {
		t.Fatalf("replayed TOTP code: %v", err)
	}

	// Wrong codes burn the challenge after MFAChallengeMaxAttempts.
	ch = loginChallenge(t, svc, "ada@example.com")
	f.advance(30 * time.Second)
	good := codeAt(t, enr.Secret, *f.clock)
	bad := "000000"
	if good == bad {
		bad = "111111"
	}
	for i := 0; i < MFAChallengeMaxAttempts; i++ {
		if _, err := svc.VerifyMFA(ctx, ch, bad); !errors.Is(err, ErrMFAChallengeInvalid) {
			t.Fatalf("bad code %d: %v", i, err)
		}
	}
	if _, err := svc.VerifyMFA(ctx, ch, good); !errors.Is(err, ErrMFAChallengeInvalid) {
		t.Fatalf("burned challenge accepted a good code: %v", err)
	}

	// Backup code works exactly once (any case / separators).
	ch = loginChallenge(t, svc, "ada@example.com")
	if _, err := svc.VerifyMFA(ctx, ch, strings.ToUpper(enr.BackupCodes[0])); err != nil {
		t.Fatalf("backup code: %v", err)
	}
	ch = loginChallenge(t, svc, "ada@example.com")
	if _, err := svc.VerifyMFA(ctx, ch, enr.BackupCodes[0]); !errors.Is(err, ErrMFAChallengeInvalid) {
		t.Fatalf("backup code reused: %v", err)
	}

	// Challenge expires.
	ch = loginChallenge(t, svc, "ada@example.com")
	f.advance(MFAChallengeTTL + time.Second)
	if _, err := svc.VerifyMFA(ctx, ch, codeAt(t, enr.Secret, *f.clock)); !errors.Is(err, ErrMFAChallengeInvalid) {
		t.Fatalf("expired challenge: %v", err)
	}

	// Disable requires the password.
	if err := svc.DisableMFA(ctx, id, "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("disable wrong password: %v", err)
	}
	if err := svc.DisableMFA(ctx, id, "password123"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(ctx, "ada@example.com", "password123"); err != nil {
		t.Fatalf("login after disable: %v", err)
	}
}

func TestMFANotConfigured(t *testing.T) {
	svc, err := NewService(newTestDB(t), testSecret())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := svc.SignUp(context.Background(), "B", "b@example.com", "password123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EnrollMFA(context.Background(), sess.Human.ID); !errors.Is(err, ErrMFANotConfigured) {
		t.Fatalf("got %v", err)
	}
}
