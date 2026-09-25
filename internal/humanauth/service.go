package humanauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
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

// ErrOrgInvitationInvalid covers every way an org invitation token can fail
// to accept - unknown, expired, already accepted, or lost a concurrent
// consume-race - deliberately never distinguished to the caller, same
// posture as ErrPasswordResetTokenInvalid.
var ErrOrgInvitationInvalid = errors.New("humanauth: org invitation invalid or expired")

// ErrOrgInvitationEmailMismatch is returned when an already-logged-in
// human tries to accept an invitation addressed to a different email.
var ErrOrgInvitationEmailMismatch = errors.New("humanauth: this invitation was sent to a different email address")

// ErrNotOrgOwner is returned when a non-owner tries to send an org
// invitation — sending is owner-only (operator decision; see DEC-205's
// deferral of a full role matrix).
var ErrNotOrgOwner = errors.New("humanauth: only an organization owner can send invitations")

const (
	// AccessTokenTTL is short-lived per the JWT model agreed with the
	// operator: ~15 minutes.
	AccessTokenTTL = 15 * time.Minute
	// RefreshTokenTTL is the longer-lived, rotating, revocable session token.
	RefreshTokenTTL = 30 * 24 * time.Hour
	// PasswordResetTokenTTL is deliberately short - the operator's own
	// stated requirement: unused within 5 minutes, it must not work.
	PasswordResetTokenTTL = 5 * time.Minute
	// OrgInvitationTTL is the operator's own stated requirement: unused
	// within 5 hours, an invitation must not work (much longer than a
	// password reset link since it's a lower-risk action shared over
	// whatever channel the inviter chooses, not a same-session self-serve
	// flow).
	OrgInvitationTTL = 5 * time.Hour
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
	// Truncated to microsecond precision to match PostgreSQL's timestamptz,
	// which has no nanosecond component: a Go time.Time compared with
	// .Equal() against one that round-tripped through the database would
	// otherwise never match (this broke CI - see humans_test.go's helpers).
	s := &Service{db: db, jwtSecret: jwtSecret, now: func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }}
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
	_, actualLastLoginAt, err := s.db.TouchLoginAndCreateRefreshToken(ctx, h.ID, loginAt, hashRawToken(raw), loginAt.Add(RefreshTokenTTL))
	if err != nil {
		return Session{}, fmt.Errorf("humanauth: record login: %w", err)
	}
	// Use what the database actually stored, not loginAt: a concurrent
	// login that reached the database first can win the monotonic guard,
	// in which case echoing our own loginAt back would tell this caller a
	// last-login time older than what is really stored (Greptile P2).
	h.LastLoginAt = &actualLastLoginAt
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

	access, err := s.issueAccessToken(h)
	if err != nil {
		return Session{}, err
	}
	raw, err := generateRawToken()
	if err != nil {
		return Session{}, err
	}
	// Revoke-old and insert-new happen in ONE transaction (database.RotateRefreshToken)
	// so a concurrent ResetPassword's "revoke every active token" can never land in the
	// gap between them and miss the replacement - see that method's doc for why the two
	// statements being independent was itself the bug (Greptile P1, PR #22).
	if _, err := s.db.RotateRefreshToken(ctx, rec.ID, h.ID, hashRawToken(raw), now, now.Add(RefreshTokenTTL)); err != nil {
		if errors.Is(err, database.ErrRefreshTokenRotationLost) {
			// Lost a concurrent rotation race (another Refresh, a Logout, or
			// a password reset already revoked this exact token). Do NOT
			// mint a session here — minting one would let a single-use
			// refresh token produce two valid sessions, or survive a reset
			// that was supposed to end it.
			return Session{}, ErrRefreshTokenInvalid
		}
		return Session{}, fmt.Errorf("humanauth: rotate refresh token: %w", err)
	}
	return Session{Human: h, AccessToken: access, RefreshToken: raw}, nil
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
	if s.dashboardBaseURL == "" {
		// A mailer without a dashboard URL would send a relative
		// "/reset-password?token=..." link the recipient has no host to
		// resolve against, i.e. an unusable email (Greptile P1, PR #22).
		// Treat this the same as no mailer at all rather than sending it.
		return fmt.Errorf("humanauth: mailer is configured but dashboard base URL is not; refusing to send an unusable reset link")
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

// InviteToOrganization issues a 5-hour org invitation and emails it.
// Sending is owner-only (returns ErrNotOrgOwner otherwise) — inviting
// someone needs only their email, no separate invite-code flow; the email
// itself carries the accept link. Mirrors ForgotPassword's shape but,
// unlike it, is NOT anti-enumeration: the caller is already an
// authenticated org owner deliberately inviting a specific address, so
// there is nothing to hide from them.
func (s *Service) InviteToOrganization(ctx context.Context, inviterHumanID, tenantID, email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return fmt.Errorf("humanauth: invitee email is required")
	}
	isOwner, err := s.db.IsTenantOwner(ctx, tenantID, inviterHumanID)
	if err != nil {
		return fmt.Errorf("humanauth: check org owner: %w", err)
	}
	if !isOwner {
		return ErrNotOrgOwner
	}
	tenant, err := s.db.GetTenant(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("humanauth: get tenant: %w", err)
	}
	inviter, err := s.db.GetHuman(ctx, inviterHumanID)
	if err != nil {
		return fmt.Errorf("humanauth: get inviter: %w", err)
	}
	raw, err := generateRawToken()
	if err != nil {
		return err
	}
	if s.mailer == nil {
		return fmt.Errorf("humanauth: no mailer is configured; cannot send an org invitation")
	}
	if s.dashboardBaseURL == "" {
		// A mailer without a dashboard URL would send a relative
		// "/accept-invite?token=..." link the recipient has no host to
		// resolve against - same fix as ForgotPassword's (Greptile P1, PR #23).
		return fmt.Errorf("humanauth: mailer is configured but dashboard base URL is not; refusing to send an unusable invitation link")
	}
	inv, err := s.db.CreateOrgInvitation(ctx, tenantID, inviterHumanID, email, hashRawToken(raw), s.now().Add(OrgInvitationTTL))
	if err != nil {
		return fmt.Errorf("humanauth: create org invitation: %w", err)
	}
	link := s.dashboardBaseURL + "/accept-invite?token=" + raw
	subject := fmt.Sprintf("%s invites you to join the organization", tenant.Name)
	text := fmt.Sprintf("%s (%s) has invited you to join %s on MailX.\n\n"+
		"Accept the invitation within 5 hours:\n%s\n\n"+
		"If you weren't expecting this, you can safely ignore this email.",
		inviter.Name, inviter.Email, tenant.Name, link)
	html := orgInvitationHTML(tenant, inviter, link)
	if err := s.mailer.SendSystemEmail(ctx, email, subject, text, html); err != nil {
		// Deliberately do NOT supersede any prior pending invitation here:
		// this new one was never delivered, so invalidating an older,
		// still-working link would leave the invitee with nothing usable
		// at all (Greptile P1, PR #23).
		return fmt.Errorf("humanauth: send org invitation email: %w", err)
	}
	// Only now that the new link is confirmed delivered is it safe to kill
	// any other pending invitation to the same address - see
	// SupersedeOtherPendingOrgInvitations's doc.
	if err := s.db.SupersedeOtherPendingOrgInvitations(ctx, tenantID, email, inv.ID, inv.CreatedAt, s.now()); err != nil {
		return fmt.Errorf("humanauth: supersede prior invitations: %w", err)
	}
	return nil
}

// orgInvitationHTML renders the invite email body: org logo + inviter
// avatar when set (plain <img> tags against the URL-only fields — see
// migration 000030's doc; no image processing/hosting here), org name,
// and the accept link. Either image is omitted entirely when its URL is
// unset, rather than showing a broken-image placeholder.
func orgInvitationHTML(tenant database.Tenant, inviter database.Human, link string) string {
	// tenant.Name/inviter.Name/inviter.Email are owner/user-supplied and end
	// up in an email MailX itself sends - html.EscapeString for both text
	// nodes and attribute values (Go's html/template would double-escape
	// the pre-built <img>/<a> markup here, so plain escaping of each
	// interpolated value is used instead, same as the rest of this file's
	// string-building style). Unescaped HTML here would let an org owner
	// inject deceptive content/links into mail MailX sends on their behalf
	// (Greptile P1/security, PR #23).
	name := html.EscapeString(tenant.Name)
	var b strings.Builder
	b.WriteString("<div>")
	if tenant.LogoURL != nil && *tenant.LogoURL != "" {
		fmt.Fprintf(&b, `<img src="%s" alt="%s logo" height="48" style="display:block;margin-bottom:8px">`, html.EscapeString(*tenant.LogoURL), name)
	}
	if inviter.AvatarURL != nil && *inviter.AvatarURL != "" {
		fmt.Fprintf(&b, `<img src="%s" alt="%s" width="32" height="32" style="border-radius:50%%;display:block;margin-bottom:8px">`, html.EscapeString(*inviter.AvatarURL), html.EscapeString(inviter.Name))
	}
	fmt.Fprintf(&b, "<p><strong>%s</strong> invites you to join the organization.</p>", name)
	fmt.Fprintf(&b, `<p><a href="%s">Accept the invitation</a> within 5 hours.</p>`, html.EscapeString(link))
	b.WriteString("<p>If you weren't expecting this, you can safely ignore this email.</p>")
	b.WriteString("</div>")
	return b.String()
}

// AcceptOrgInvitationResult is what AcceptOrgInvitation hands back: the
// tenant joined, and a fresh session only when a new account was created
// (an already-logged-in caller keeps using their existing session — see
// AcceptOrgInvitation's doc).
type AcceptOrgInvitationResult struct {
	Tenant  database.Tenant
	Session *Session // non-nil only when a new account was created by this call
}

// AcceptOrgInvitation accepts a pending invitation identified by rawToken.
// Handles both cases the invitee can be in (operator decision: one
// combined endpoint, not separate signup-then-join calls):
//
//   - existingHumanID != "": the caller already has a session. The
//     invitation's email must match that account's own email
//     (case-insensitive) — otherwise ErrOrgInvitationEmailMismatch, since
//     accepting someone else's invitation under your own account would
//     silently misattribute membership.
//   - existingHumanID == "": the caller has no account yet. signupName and
//     signupPassword create one, atomically joined to the org in the same
//     transaction (database.AcceptOrgInvitationWithSignup) — the account's
//     email is ALWAYS the invitation's own address, never client-supplied,
//     so an invite token for one address can never mint an account under
//     another.
func (s *Service) AcceptOrgInvitation(ctx context.Context, rawToken, existingHumanID, signupName, signupPassword string) (AcceptOrgInvitationResult, error) {
	inv, err := s.db.GetOrgInvitationByHash(ctx, hashRawToken(rawToken))
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return AcceptOrgInvitationResult{}, ErrOrgInvitationInvalid
		}
		return AcceptOrgInvitationResult{}, fmt.Errorf("humanauth: get org invitation: %w", err)
	}
	now := s.now()
	if inv.AcceptedAt != nil || !inv.ExpiresAt.After(now) {
		return AcceptOrgInvitationResult{}, ErrOrgInvitationInvalid
	}
	tenant, err := s.db.GetTenant(ctx, inv.TenantID)
	if err != nil {
		return AcceptOrgInvitationResult{}, fmt.Errorf("humanauth: get tenant: %w", err)
	}

	if existingHumanID != "" {
		h, err := s.db.GetHuman(ctx, existingHumanID)
		if err != nil {
			return AcceptOrgInvitationResult{}, fmt.Errorf("humanauth: get human: %w", err)
		}
		if normalizeEmailForCompare(h.Email) != inv.NormalizedEmail {
			return AcceptOrgInvitationResult{}, ErrOrgInvitationEmailMismatch
		}
		if err := s.db.AcceptOrgInvitationForExistingHuman(ctx, inv.ID, inv.TenantID, existingHumanID, now); err != nil {
			if errors.Is(err, database.ErrOrgInvitationConsumed) {
				return AcceptOrgInvitationResult{}, ErrOrgInvitationInvalid
			}
			return AcceptOrgInvitationResult{}, fmt.Errorf("humanauth: accept org invitation: %w", err)
		}
		return AcceptOrgInvitationResult{Tenant: tenant}, nil
	}

	name := strings.TrimSpace(signupName)
	if name == "" || len(signupPassword) < 8 {
		return AcceptOrgInvitationResult{}, fmt.Errorf("humanauth: name and a password of at least 8 characters are required to accept this invitation")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(signupPassword), bcrypt.DefaultCost)
	if err != nil {
		return AcceptOrgInvitationResult{}, fmt.Errorf("humanauth: hash password: %w", err)
	}
	h, err := s.db.AcceptOrgInvitationWithSignup(ctx, inv.ID, inv.TenantID, name, inv.RawEmail, string(hash), now)
	if err != nil {
		if errors.Is(err, database.ErrOrgInvitationConsumed) {
			return AcceptOrgInvitationResult{}, ErrOrgInvitationInvalid
		}
		if errors.Is(err, database.ErrConflict) {
			return AcceptOrgInvitationResult{}, ErrEmailTaken
		}
		return AcceptOrgInvitationResult{}, fmt.Errorf("humanauth: accept org invitation with signup: %w", err)
	}
	session, err := s.mintSession(ctx, h)
	if err != nil {
		return AcceptOrgInvitationResult{}, err
	}
	return AcceptOrgInvitationResult{Tenant: tenant, Session: &session}, nil
}

// normalizeEmailForCompare mirrors database's unexported normalizeEmail
// (lowercase + trim) — duplicated here rather than exported across the
// package boundary for one comparison.
func normalizeEmailForCompare(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
