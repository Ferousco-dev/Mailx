package humanauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"golang.org/x/crypto/bcrypt"
)

// ErrInvalidCredentials is returned by Login for both "no such email" and
// "wrong password" — deliberately identical so the API layer can never
// leak which one it was (anti email-enumeration).
var ErrInvalidCredentials = errors.New("humanauth: invalid credentials")

// ErrEmailTaken is returned by SignUp on a duplicate (case-insensitive) email.
var ErrEmailTaken = errors.New("humanauth: email already registered")

// ErrRefreshTokenInvalid covers expired, revoked, or unknown refresh tokens.
var ErrRefreshTokenInvalid = errors.New("humanauth: refresh token invalid or expired")

// ErrPasswordResetTokenInvalid covers every way a password reset token can
// fail - unknown, expired, already used, or lost a concurrent
// consume-race - deliberately never distinguished to the caller, the same
// anti-enumeration posture as ErrInvalidCredentials.
var ErrPasswordResetTokenInvalid = errors.New("humanauth: password reset token invalid or expired")

const (
	// AccessTokenTTL is short-lived per the JWT model agreed with the
	// operator: ~15 minutes.
	AccessTokenTTL = 15 * time.Minute
	// RefreshTokenTTL is the longer-lived, rotating, revocable session token.
	RefreshTokenTTL = 30 * 24 * time.Hour
	// PasswordResetTokenTTL is deliberately short - the operator's own
	// stated requirement: unused within 5 minutes, it must not work.
	PasswordResetTokenTTL = 5 * time.Minute
)

// Mailer sends a system-originated email to a human account holder
// (password reset today; verification/notification later). Kept as a
// small interface here rather than importing internal/api directly:
// internal/api already imports this package for its HTTP handlers, so the
// reverse import would cycle. cmd/mailx wires a concrete implementation
// backed by api.SubmissionAcceptor - MailX sends its OWN account emails
// through the exact same durable, DKIM-signed, suppression-aware pipeline
// every tenant's mail goes through, not a separate ad hoc code path.
type Mailer interface {
	SendSystemEmail(ctx context.Context, to, subject, text, html string) error
}

// Option configures an optional Service dependency.
type Option func(*Service)

// WithMailer enables ForgotPassword to actually send the reset email. Without
// one, ForgotPassword still creates the token (so a future non-email delivery
// path could use it) but cannot deliver it - logged by the caller, not an error
// returned to the end user (who must never learn whether their email exists
// from this response either way).
func WithMailer(m Mailer) Option { return func(s *Service) { s.mailer = m } }

// WithDashboardBaseURL sets the origin used to build the reset link sent by
// email, e.g. "https://app.mailx.dev" -> ".../reset-password?token=...".
func WithDashboardBaseURL(url string) Option { return func(s *Service) { s.dashboardBaseURL = url } }

// WithNow overrides the clock, for tests that need to exercise TTL expiry
// (e.g. password reset tokens) without sleeping.
func WithNow(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// Session is what SignUp/Login/Refresh hand back to the caller.
type Session struct {
	Human        database.Human
	AccessToken  string
	RefreshToken string
}

// Service is the human-auth equivalent of internal/auth.Service — a
// PostgreSQL-backed lifecycle, kept as a separate type so it can never be
// passed where an API-key authService is expected.
type Service struct {
	db               *database.DB
	jwtSecret        []byte
	now              func() time.Time
	mailer           Mailer
	dashboardBaseURL string
}

// NewService constructs a Service. jwtSecret must be non-empty — callers
// read it from MAILX_JWT_SECRET (see cmd/mailx), matching this codebase's
// existing secret-env-var convention.
func NewService(db *database.DB, jwtSecret []byte, opts ...Option) (*Service, error) {
	if len(jwtSecret) == 0 {
		return nil, fmt.Errorf("humanauth: jwt secret is empty")
	}
	s := &Service{db: db, jwtSecret: jwtSecret, now: func() time.Time { return time.Now().UTC() }}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

func generateRawToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("humanauth: generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func hashRawToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (s *Service) issueAccessToken(h database.Human) (string, error) {
	now := s.now()
	claims := Claims{
		HumanID: h.ID,
		Role:    h.Role,
		IatUnix: now.Unix(),
		ExpUnix: now.Add(AccessTokenTTL).Unix(),
	}
	return signJWT(s.jwtSecret, claims)
}

func (s *Service) issueRefreshToken(ctx context.Context, humanID string) (string, error) {
	raw, err := generateRawToken()
	if err != nil {
		return "", err
	}
	_, err = s.db.CreateRefreshToken(ctx, humanID, hashRawToken(raw), s.now().Add(RefreshTokenTTL))
	if err != nil {
		return "", fmt.Errorf("humanauth: create refresh token: %w", err)
	}
	return raw, nil
}

func (s *Service) mintSession(ctx context.Context, h database.Human) (Session, error) {
	access, err := s.issueAccessToken(h)
	if err != nil {
		return Session{}, err
	}
	refresh, err := s.issueRefreshToken(ctx, h.ID)
	if err != nil {
		return Session{}, err
	}
	return Session{Human: h, AccessToken: access, RefreshToken: refresh}, nil
}

// SignUp creates a new human account and an initial session. Returns
// ErrEmailTaken on a duplicate (case-insensitive) email.
func (s *Service) SignUp(ctx context.Context, name, email, password string) (Session, error) {
	name = strings.TrimSpace(name)
	email = strings.TrimSpace(email)
	if name == "" || email == "" || len(password) < 8 {
		return Session{}, fmt.Errorf("humanauth: name, email, and a password of at least 8 characters are required")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return Session{}, fmt.Errorf("humanauth: hash password: %w", err)
	}
	h, err := s.db.CreateHuman(ctx, name, email, string(hash))
	if err != nil {
		if errors.Is(err, database.ErrConflict) {
			return Session{}, ErrEmailTaken
		}
		return Session{}, fmt.Errorf("humanauth: create human: %w", err)
	}
	return s.mintSession(ctx, h)
}

// Login authenticates by email+password. Both "no such email" and "wrong
// password" return the same ErrInvalidCredentials — never distinguish
// them, and bcrypt is always run on SOME hash (a fixed dummy one when the
// email doesn't exist) so failure timing doesn't leak account existence
// either.
var dummyBcryptHash = mustHash("humanauth-dummy-password-for-timing-parity")

func mustHash(pw string) []byte {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return h
}

func (s *Service) Login(ctx context.Context, email, password string) (Session, error) {
	h, err := s.db.GetHumanByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(password))
			return Session{}, ErrInvalidCredentials
		}
		return Session{}, fmt.Errorf("humanauth: get human: %w", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(h.PasswordHash), []byte(password)); err != nil {
		return Session{}, ErrInvalidCredentials
	}
	// Deliberately NOT mintSession here: recording the login and issuing
	// its refresh token must succeed or fail TOGETHER (see
	// TouchLoginAndCreateRefreshToken's doc) - a plain mintSession call
	// would let a refresh-token-insert failure leave the login recorded
	// as successful for an attempt that returned no session.
	access, err := s.issueAccessToken(h)
	if err != nil {
		return Session{}, err
	}
	raw, err := generateRawToken()
	if err != nil {
		return Session{}, err
	}
	loginAt := s.now()
	if _, err := s.db.TouchLoginAndCreateRefreshToken(ctx, h.ID, loginAt, hashRawToken(raw), loginAt.Add(RefreshTokenTTL)); err != nil {
		return Session{}, fmt.Errorf("humanauth: record login: %w", err)
	}
	h.LastLoginAt = &loginAt // reflect the just-recorded touch, avoiding a re-fetch
	return Session{Human: h, AccessToken: access, RefreshToken: raw}, nil
}

// Refresh validates and rotates a refresh token: the old token is
// revoked and a new access+refresh pair is issued. Reuse of an
// already-revoked token is treated as a compromise signal and revokes
// every active refresh token for that human (session-wide).
func (s *Service) Refresh(ctx context.Context, rawRefreshToken string) (Session, error) {
	rec, err := s.db.GetRefreshTokenByHash(ctx, hashRawToken(rawRefreshToken))
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return Session{}, ErrRefreshTokenInvalid
		}
		return Session{}, fmt.Errorf("humanauth: get refresh token: %w", err)
	}
	now := s.now()
	if rec.RevokedAt != nil {
		// Reuse of a token already revoked (rotated away, or explicitly
		// logged out) — treat as compromise: kill the whole session.
		if err := s.db.RevokeAllRefreshTokensForHuman(ctx, rec.HumanID, now); err != nil {
			return Session{}, fmt.Errorf("humanauth: revoke all on reuse: %w", err)
		}
		return Session{}, ErrRefreshTokenInvalid
	}
	if !rec.ExpiresAt.After(now) {
		return Session{}, ErrRefreshTokenInvalid
	}

	h, err := s.db.GetHuman(ctx, rec.HumanID)
	if err != nil {
		return Session{}, fmt.Errorf("humanauth: get human: %w", err)
	}

	revoked, err := s.db.RevokeRefreshToken(ctx, rec.ID, now)
	if err != nil {
		return Session{}, fmt.Errorf("humanauth: revoke old refresh token: %w", err)
	}
	if !revoked {
		// Lost a concurrent rotation race: another request already
		// revoked this exact token between our read above and this
		// UPDATE. Do NOT mint a session here — the winner of the race
		// already got one, and minting a second would let one refresh
		// token produce two valid sessions.
		return Session{}, ErrRefreshTokenInvalid
	}
	return s.mintSession(ctx, h)
}

// Logout revokes the given refresh token (idempotent).
func (s *Service) Logout(ctx context.Context, rawRefreshToken string) error {
	rec, err := s.db.GetRefreshTokenByHash(ctx, hashRawToken(rawRefreshToken))
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("humanauth: get refresh token: %w", err)
	}
	_, err = s.db.RevokeRefreshToken(ctx, rec.ID, s.now())
	return err
}

// ForgotPassword issues a 5-minute password reset token and emails it, if
// an account exists for email - but the caller (the API handler) must
// ALWAYS present the same generic response regardless of the outcome
// here, exactly as Login never reveals whether an email is registered.
// Returns nil for a genuinely unknown email (not an error - there is
// nothing to do, which is the whole point); returns a non-nil error only
// for a real infrastructure failure (DB down, mailer failure), which the
// caller should log but still must not surface to the requester.
func (s *Service) ForgotPassword(ctx context.Context, email string) error {
	h, err := s.db.GetHumanByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("humanauth: get human: %w", err)
	}
	raw, err := generateRawToken()
	if err != nil {
		return err
	}
	if _, err := s.db.CreatePasswordResetToken(ctx, h.ID, hashRawToken(raw), s.now().Add(PasswordResetTokenTTL)); err != nil {
		return fmt.Errorf("humanauth: create password reset token: %w", err)
	}
	if s.mailer == nil {
		// The token exists durably even with no mailer configured (an
		// operator could still hand it out some other way later), but
		// there is no delivery path right now - the caller logs this,
		// the requester still sees the same generic success response.
		return fmt.Errorf("humanauth: password reset token created but no mailer is configured")
	}
	link := s.dashboardBaseURL + "/reset-password?token=" + raw
	text := "Someone requested a password reset for your MailX account.\n\n" +
		"If this was you, reset your password within 5 minutes:\n" + link + "\n\n" +
		"If you didn't request this, you can safely ignore this email - your password has not been changed."
	html := `<p>Someone requested a password reset for your MailX account.</p>` +
		`<p>If this was you, <a href="` + link + `">reset your password</a> within 5 minutes.</p>` +
		`<p>If you didn't request this, you can safely ignore this email — your password has not been changed.</p>`
	if err := s.mailer.SendSystemEmail(ctx, h.Email, "Reset your MailX password", text, html); err != nil {
		return fmt.Errorf("humanauth: send password reset email: %w", err)
	}
	return nil
}

// ResetPassword consumes a password reset token and sets a new password.
// Revokes every active refresh token for the account in the same
// transaction as the password change (database.DB.ResetPassword) - a
// password reset must end every other existing session, the same
// compromise-response posture already used for detected refresh-token
// reuse (DEC-208).
func (s *Service) ResetPassword(ctx context.Context, rawToken, newPassword string) error {
	if len(newPassword) < 8 {
		return fmt.Errorf("humanauth: password must be at least 8 characters")
	}
	rec, err := s.db.GetPasswordResetTokenByHash(ctx, hashRawToken(rawToken))
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return ErrPasswordResetTokenInvalid
		}
		return fmt.Errorf("humanauth: get password reset token: %w", err)
	}
	now := s.now()
	if rec.UsedAt != nil || !rec.ExpiresAt.After(now) {
		return ErrPasswordResetTokenInvalid
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("humanauth: hash password: %w", err)
	}
	if err := s.db.ResetPassword(ctx, rec.ID, rec.HumanID, string(hash), now); err != nil {
		if errors.Is(err, database.ErrPasswordResetTokenConsumed) {
			return ErrPasswordResetTokenInvalid
		}
		return fmt.Errorf("humanauth: reset password: %w", err)
	}
	return nil
}

// VerifyAccessToken validates a JWT access token and returns its claims,
// for use as HTTP middleware.
func (s *Service) VerifyAccessToken(token string) (Claims, error) {
	claims, err := verifyJWT(s.jwtSecret, token)
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	return claims, nil
}

// CreateOrganization wraps database.CreateOrganization (tenant + owner
// membership, atomic).
func (s *Service) CreateOrganization(ctx context.Context, humanID, name, slug string) (database.Tenant, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return database.Tenant{}, fmt.Errorf("humanauth: organization name is required")
	}
	// slug is accepted for the frontend contract ({name, slug}) but has
	// no dedicated column yet — tenants has no slug column, and adding
	// one is out of scope for this phase; kept as a no-op input for now
	// rather than silently dropped without acknowledgment.
	_ = slug
	return s.db.CreateOrganization(ctx, humanID, name)
}

// ListOrganizationsForHuman returns only the tenants the human belongs to.
func (s *Service) ListOrganizationsForHuman(ctx context.Context, humanID string) ([]database.Tenant, error) {
	return s.db.ListOrganizationsForHuman(ctx, humanID)
}
