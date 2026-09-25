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

const (
	// AccessTokenTTL is short-lived per the JWT model agreed with the
	// operator: ~15 minutes.
	AccessTokenTTL = 15 * time.Minute
	// RefreshTokenTTL is the longer-lived, rotating, revocable session token.
	RefreshTokenTTL = 30 * 24 * time.Hour
)

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
	db        *database.DB
	jwtSecret []byte
	now       func() time.Time
}

// NewService constructs a Service. jwtSecret must be non-empty — callers
// read it from MAILX_JWT_SECRET (see cmd/mailx), matching this codebase's
// existing secret-env-var convention.
func NewService(db *database.DB, jwtSecret []byte) (*Service, error) {
	if len(jwtSecret) == 0 {
		return nil, fmt.Errorf("humanauth: jwt secret is empty")
	}
	return &Service{db: db, jwtSecret: jwtSecret, now: func() time.Time { return time.Now().UTC() }}, nil
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
	return s.mintSession(ctx, h)
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

	if err := s.db.RevokeRefreshToken(ctx, rec.ID, now); err != nil {
		return Session{}, fmt.Errorf("humanauth: revoke old refresh token: %w", err)
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
	return s.db.RevokeRefreshToken(ctx, rec.ID, s.now())
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
