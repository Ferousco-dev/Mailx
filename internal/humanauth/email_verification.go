package humanauth

import (
	"context"
	"errors"
	"fmt"
	"html"

	"github.com/Ferousco-dev/mailx/internal/database"
)

// ErrEmailVerificationTokenInvalid covers every way an email verification
// token can fail - unknown, expired, already used, superseded by a resend,
// or lost a concurrent consume-race - never distinguished to the caller,
// same posture as ErrPasswordResetTokenInvalid.
var ErrEmailVerificationTokenInvalid = errors.New("humanauth: email verification token invalid or expired")

// sendVerificationEmail issues a fresh 15-minute verification token for h
// (superseding any pending one, DEC-242) and emails it. Returns nil and
// does nothing if h is already verified. Errors are infrastructure
// failures the caller logs; they must never fail SignUp or be surfaced
// by ResendVerification.
func (s *Service) sendVerificationEmail(ctx context.Context, h database.Human) error {
	if s.mailer == nil {
		return fmt.Errorf("humanauth: no mailer is configured; cannot send a verification email")
	}
	if s.dashboardBaseURL == "" {
		// Same guard as ForgotPassword (DEC-215): never send a relative,
		// unusable link.
		return fmt.Errorf("humanauth: mailer is configured but dashboard base URL is not; refusing to send an unusable verification link")
	}
	raw, err := generateRawToken()
	if err != nil {
		return err
	}
	now := s.now()
	t, err := s.db.CreateEmailVerificationToken(ctx, h.ID, hashRawToken(raw), now.Add(EmailVerificationTokenTTL))
	if err != nil {
		if errors.Is(err, database.ErrEmailAlreadyVerified) {
			return nil
		}
		return fmt.Errorf("humanauth: issue email verification token: %w", err)
	}
	link := s.dashboardBaseURL + "/verify-email?token=" + raw
	text := "Hello " + h.Name + ", welcome to MailX!\n\n" +
		"To finish setting up your account, verify your email within 15 minutes:\n" + link + "\n\n" +
		"If you didn't create a MailX account, you can safely ignore this email."
	body := `<p>Hello ` + html.EscapeString(h.Name) + `, welcome to MailX!</p>` +
		`<p>To finish setting up your account, click the button below within 15 minutes to verify your email.</p>` +
		`<p><a href="` + link + `" style="display:inline-block;padding:10px 18px;background:#111;color:#fff;text-decoration:none;border-radius:6px">Verify my account</a></p>` +
		`<p>If you didn't create a MailX account, you can safely ignore this email.</p>`
	if err := s.mailer.SendSystemEmail(ctx, h.Email, "Verify your MailX account", text, body); err != nil {
		// Deliberately do NOT supersede prior tokens here: this new one was
		// never delivered, so invalidating an older, still-working link
		// would leave the account holder with nothing usable at all
		// (same fix shape as DEC-218, PR #23).
		return fmt.Errorf("humanauth: send verification email: %w", err)
	}
	// Only now that the new link is confirmed delivered is it safe to
	// invalidate any other pending token - see
	// SupersedeOtherEmailVerificationTokens's doc.
	if err := s.db.SupersedeOtherEmailVerificationTokens(ctx, h.ID, t.ID, t.CreatedAt, s.now()); err != nil {
		return fmt.Errorf("humanauth: supersede prior verification tokens: %w", err)
	}
	return nil
}

// ResendVerification issues a new verification email if an UNVERIFIED
// account exists for email. The caller must ALWAYS present the same
// generic response (anti-enumeration, same as ForgotPassword): unknown
// and already-verified emails return nil; a non-nil error is an
// infrastructure failure to log, never to surface.
func (s *Service) ResendVerification(ctx context.Context, email string) error {
	h, err := s.db.GetHumanByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("humanauth: get human: %w", err)
	}
	if h.EmailVerifiedAt != nil {
		return nil
	}
	return s.sendVerificationEmail(ctx, h)
}

// VerifyEmail consumes a verification token and marks the account
// verified (database.DB.VerifyEmail - atomic, RowsAffected-checked).
func (s *Service) VerifyEmail(ctx context.Context, rawToken string) error {
	rec, err := s.db.GetEmailVerificationTokenByHash(ctx, hashRawToken(rawToken))
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return ErrEmailVerificationTokenInvalid
		}
		return fmt.Errorf("humanauth: get email verification token: %w", err)
	}
	now := s.now()
	if rec.UsedAt != nil || !rec.ExpiresAt.After(now) {
		return ErrEmailVerificationTokenInvalid
	}
	if err := s.db.VerifyEmail(ctx, rec.ID, rec.HumanID, now); err != nil {
		if errors.Is(err, database.ErrEmailVerificationTokenConsumed) {
			return ErrEmailVerificationTokenInvalid
		}
		return fmt.Errorf("humanauth: verify email: %w", err)
	}
	return nil
}
