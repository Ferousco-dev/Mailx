package humanauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/secretbox"
	"golang.org/x/crypto/bcrypt"
)

// TOTP MFA for human accounts (DEC-229/DEC-230).

const (
	// MFAChallengeTTL bounds how long a password-verified login may wait
	// for its second factor.
	MFAChallengeTTL = 5 * time.Minute
	// MFAChallengeMaxAttempts burns a challenge after this many wrong codes,
	// independent of the per-IP rate limit.
	MFAChallengeMaxAttempts = 5
	backupCodeCount         = 10
	totpSecretBytes         = 20
	totpIssuer              = "MailX"
)

var (
	// ErrMFANotConfigured means MAILX_MFA_MASTER_KEY is unset, so TOTP
	// secrets cannot be sealed or opened.
	ErrMFANotConfigured = errors.New("humanauth: mfa is not configured on this server")
	// ErrMFAAlreadyEnabled is returned by EnrollMFA when MFA is active.
	ErrMFAAlreadyEnabled = errors.New("humanauth: mfa is already enabled; disable it first")
	// ErrMFANotPending is returned by ConfirmMFA without a pending enrollment.
	ErrMFANotPending = errors.New("humanauth: no pending mfa enrollment")
	// ErrMFACodeInvalid is returned for a wrong TOTP code on confirm.
	ErrMFACodeInvalid = errors.New("humanauth: invalid mfa code")
	// ErrMFAChallengeInvalid covers every failure of VerifyMFA (unknown,
	// expired, used, burned challenge, or wrong/replayed code) - never
	// distinguished to the caller.
	ErrMFAChallengeInvalid = errors.New("humanauth: mfa challenge invalid, expired, or code incorrect")
)

// MFARequiredError is returned by Login (and the OAuth callback) instead of a
// session when the account has MFA enabled. ChallengeToken is an opaque,
// single-use, MFAChallengeTTL-lived value that is ONLY accepted by VerifyMFA:
// it is not a JWT and never verifies as an access token.
type MFARequiredError struct {
	ChallengeToken string
	ExpiresAt      time.Time
}

func (e *MFARequiredError) Error() string { return "humanauth: mfa required" }

// WithMFABox enables TOTP enrollment/verification with a secretbox keyed by
// MAILX_MFA_MASTER_KEY.
func WithMFABox(b *secretbox.Box) Option { return func(s *Service) { s.mfaBox = b } }

// MFAConfigured reports whether TOTP secrets can be sealed/opened.
func (s *Service) MFAConfigured() bool { return s.mfaBox != nil }

func mfaAD(humanID string) []byte { return []byte("mfa:" + humanID) }

// normalizeBackupCode lowercases and strips separators/spaces.
func normalizeBackupCode(c string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToLower(strings.TrimSpace(c)))
}

func generateBackupCode() (string, error) {
	b := make([]byte, 8) // 64 bits
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	h := hex.EncodeToString(b)
	return h[0:4] + "-" + h[4:8] + "-" + h[8:12] + "-" + h[12:16], nil
}

// MFAEnrollment is returned once by EnrollMFA. Secret and BackupCodes are
// shown to the human exactly once and never retrievable again.
type MFAEnrollment struct {
	Secret      string // base32
	OTPAuthURI  string
	BackupCodes []string
}

// EnrollMFA generates a new pending TOTP secret and fresh backup codes. MFA is
// not active until ConfirmMFA succeeds.
func (s *Service) EnrollMFA(ctx context.Context, humanID string) (MFAEnrollment, error) {
	if s.mfaBox == nil {
		return MFAEnrollment{}, ErrMFANotConfigured
	}
	h, err := s.db.GetHuman(ctx, humanID)
	if err != nil {
		return MFAEnrollment{}, fmt.Errorf("humanauth: get human: %w", err)
	}
	if h.MFAEnabled {
		return MFAEnrollment{}, ErrMFAAlreadyEnabled
	}
	key := make([]byte, totpSecretBytes)
	if _, err := rand.Read(key); err != nil {
		return MFAEnrollment{}, fmt.Errorf("humanauth: generate totp secret: %w", err)
	}
	ct, nonce, err := s.mfaBox.Encrypt(key, mfaAD(humanID))
	if err != nil {
		return MFAEnrollment{}, err
	}
	codes := make([]string, backupCodeCount)
	hashes := make([]string, backupCodeCount)
	for i := range codes {
		c, err := generateBackupCode()
		if err != nil {
			return MFAEnrollment{}, err
		}
		codes[i], hashes[i] = c, hashRawToken(normalizeBackupCode(c))
	}
	if err := s.db.SetPendingMFA(ctx, humanID, ct, nonce, hashes); err != nil {
		if errors.Is(err, database.ErrConflict) {
			return MFAEnrollment{}, ErrMFAAlreadyEnabled
		}
		return MFAEnrollment{}, fmt.Errorf("humanauth: store pending mfa: %w", err)
	}
	return MFAEnrollment{Secret: totpB32.EncodeToString(key), OTPAuthURI: otpauthURI(totpIssuer, h.Email, key), BackupCodes: codes}, nil
}

// ConfirmMFA activates the pending secret if code is valid for it.
func (s *Service) ConfirmMFA(ctx context.Context, humanID, code string) error {
	if s.mfaBox == nil {
		return ErrMFANotConfigured
	}
	m, err := s.db.GetMFASecrets(ctx, humanID)
	if err != nil {
		return fmt.Errorf("humanauth: get mfa: %w", err)
	}
	if m.Enabled {
		return ErrMFAAlreadyEnabled
	}
	if m.PendingCiphertext == nil {
		return ErrMFANotPending
	}
	key, err := s.mfaBox.Decrypt(m.PendingCiphertext, m.PendingNonce, mfaAD(humanID))
	if err != nil {
		return fmt.Errorf("humanauth: open pending mfa secret: %w", err)
	}
	step, ok := verifyTOTP(key, strings.TrimSpace(code), s.now())
	if !ok {
		return ErrMFACodeInvalid
	}
	if err := s.db.ActivateMFA(ctx, humanID, m.PendingCiphertext, step); err != nil {
		if errors.Is(err, database.ErrMFAConsumed) {
			return ErrMFANotPending
		}
		return fmt.Errorf("humanauth: activate mfa: %w", err)
	}
	return nil
}

// DisableMFA removes MFA after re-confirming the account password. A wrong
// password returns ErrInvalidCredentials. Accounts created via OAuth have no
// usable password and must set one (password reset) first.
func (s *Service) DisableMFA(ctx context.Context, humanID, password string) error {
	h, err := s.db.GetHuman(ctx, humanID)
	if err != nil {
		return fmt.Errorf("humanauth: get human: %w", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(h.PasswordHash), []byte(password)); err != nil {
		return ErrInvalidCredentials
	}
	if err := s.db.DisableMFA(ctx, humanID); err != nil {
		return fmt.Errorf("humanauth: disable mfa: %w", err)
	}
	return nil
}

// newMFAChallenge issues the intermediate login state for an MFA account.
func (s *Service) newMFAChallenge(ctx context.Context, h database.Human) error {
	raw, err := generateRawToken()
	if err != nil {
		return err
	}
	exp := s.now().Add(MFAChallengeTTL)
	if err := s.db.CreateMFAChallenge(ctx, h.ID, hashRawToken(raw), exp); err != nil {
		return fmt.Errorf("humanauth: create mfa challenge: %w", err)
	}
	return &MFARequiredError{ChallengeToken: raw, ExpiresAt: exp}
}

// VerifyMFA completes an MFA login: challengeToken from MFARequiredError plus
// either a 6-digit TOTP code or a backup code. On success the challenge and the
// factor are consumed atomically and a full session is minted through the same
// path as a password-only Login.
func (s *Service) VerifyMFA(ctx context.Context, challengeToken, code string) (Session, error) {
	c, err := s.db.GetMFAChallengeByHash(ctx, hashRawToken(challengeToken))
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return Session{}, ErrMFAChallengeInvalid
		}
		return Session{}, fmt.Errorf("humanauth: get mfa challenge: %w", err)
	}
	now := s.now()
	if c.UsedAt != nil || !c.ExpiresAt.After(now) {
		return Session{}, ErrMFAChallengeInvalid
	}
	fail := func() (Session, error) {
		if err := s.db.RecordMFAChallengeFailure(ctx, c.ID, MFAChallengeMaxAttempts, now); err != nil {
			return Session{}, fmt.Errorf("humanauth: record mfa failure: %w", err)
		}
		return Session{}, ErrMFAChallengeInvalid
	}
	code = strings.TrimSpace(code)
	var step *int64
	backupHash := ""
	if len(code) == totpDigits {
		if s.mfaBox == nil {
			return Session{}, ErrMFANotConfigured
		}
		m, err := s.db.GetMFASecrets(ctx, c.HumanID)
		if err != nil {
			return Session{}, fmt.Errorf("humanauth: get mfa: %w", err)
		}
		if !m.Enabled || m.SecretCiphertext == nil {
			return Session{}, ErrMFAChallengeInvalid
		}
		key, err := s.mfaBox.Decrypt(m.SecretCiphertext, m.SecretNonce, mfaAD(c.HumanID))
		if err != nil {
			return Session{}, fmt.Errorf("humanauth: open mfa secret: %w", err)
		}
		st, ok := verifyTOTP(key, code, now)
		if !ok {
			return fail()
		}
		step = &st
	} else {
		n := normalizeBackupCode(code)
		if len(n) != 16 {
			return fail()
		}
		backupHash = hashRawToken(n)
	}
	if err := s.db.CompleteMFAChallenge(ctx, c.ID, c.HumanID, now, step, backupHash); err != nil {
		if errors.Is(err, database.ErrMFAConsumed) {
			// Replayed TOTP step, unknown/used backup code, or a lost race.
			return fail()
		}
		return Session{}, fmt.Errorf("humanauth: complete mfa challenge: %w", err)
	}
	h, err := s.db.GetHuman(ctx, c.HumanID)
	if err != nil {
		return Session{}, fmt.Errorf("humanauth: get human: %w", err)
	}
	return s.completeLogin(ctx, h)
}
