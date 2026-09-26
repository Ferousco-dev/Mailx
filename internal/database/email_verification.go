package database

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// EmailVerificationToken is one issued email-verification token row
// (migration 000034). Same shape as PasswordResetToken.
type EmailVerificationToken struct {
	ID        string
	HumanID   string
	TokenHash string
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
}

// ErrEmailAlreadyVerified is returned by IssueEmailVerificationToken when
// the human is already verified (checked under the row lock).
var ErrEmailAlreadyVerified = errors.New("database: email already verified")

// ErrEmailVerificationTokenConsumed means this exact token row was already
// used or superseded by a concurrent call - see VerifyEmail's doc.
var ErrEmailVerificationTokenConsumed = errors.New("database: email verification token already used")

// IssueEmailVerificationToken atomically supersedes (marks used) every
// still-pending verification token for humanID and inserts a fresh one,
// so at most one token is ever valid per account (DEC-242).
//
// Race safety (the DEC-226 lesson): the whole invalidate-then-insert runs
// under SELECT ... FOR UPDATE on the humans row. Two concurrent issues for
// the same human are therefore fully serialized - the second only starts
// its UPDATE after the first has committed its INSERT, so it always sees
// and supersedes the first's token, and it can never supersede its own.
// Exactly one token (the last committed) remains valid. Unlike org
// invitations there is no multi-inviter key to order by; the human row is
// a natural per-account mutex. VerifyEmail takes the same lock, so an
// issue can never interleave with a verify either.
func (db *DB) IssueEmailVerificationToken(ctx context.Context, humanID, tokenHash string, expiresAt, now time.Time) (EmailVerificationToken, error) {
	id, err := newID()
	if err != nil {
		return EmailVerificationToken{}, err
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return EmailVerificationToken{}, fmt.Errorf("database: begin issue email verification: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var verifiedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT email_verified_at FROM humans WHERE id = $1 FOR UPDATE`, humanID).Scan(&verifiedAt); err != nil {
		return EmailVerificationToken{}, normalizeErr(err)
	}
	if verifiedAt != nil {
		return EmailVerificationToken{}, ErrEmailAlreadyVerified
	}
	if _, err := tx.Exec(ctx, `UPDATE email_verification_tokens SET used_at = $2 WHERE human_id = $1 AND used_at IS NULL`, humanID, now); err != nil {
		return EmailVerificationToken{}, fmt.Errorf("database: supersede email verification tokens: %w", normalizeErr(err))
	}
	var t EmailVerificationToken
	err = tx.QueryRow(ctx, `
		INSERT INTO email_verification_tokens (id, human_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING id, human_id, token_hash, expires_at, used_at, created_at`,
		id, humanID, tokenHash, expiresAt,
	).Scan(&t.ID, &t.HumanID, &t.TokenHash, &t.ExpiresAt, &t.UsedAt, &t.CreatedAt)
	if err != nil {
		return EmailVerificationToken{}, normalizeErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return EmailVerificationToken{}, fmt.Errorf("database: commit issue email verification: %w", normalizeErr(err))
	}
	return t, nil
}

// GetEmailVerificationTokenByHash loads a token row by hash. Returns
// ErrNotFound if absent (callers check used/expired themselves).
func (db *DB) GetEmailVerificationTokenByHash(ctx context.Context, tokenHash string) (EmailVerificationToken, error) {
	var t EmailVerificationToken
	err := db.pool.QueryRow(ctx, `
		SELECT id, human_id, token_hash, expires_at, used_at, created_at
		FROM email_verification_tokens WHERE token_hash = $1`, tokenHash,
	).Scan(&t.ID, &t.HumanID, &t.TokenHash, &t.ExpiresAt, &t.UsedAt, &t.CreatedAt)
	if err != nil {
		return EmailVerificationToken{}, normalizeErr(err)
	}
	return t, nil
}

// VerifyEmail atomically marks tokenID used (only if not already used and
// not expired at now - RowsAffected-checked, same pattern as
// ResetPassword) and sets humans.email_verified_at. Both or neither.
// Keeps the first verification time if the human was somehow already
// verified. Locks the humans row first, same order as
// IssueEmailVerificationToken, so the two never deadlock or interleave.
func (db *DB) VerifyEmail(ctx context.Context, tokenID, humanID string, now time.Time) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin verify email: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT 1 FROM humans WHERE id = $1 FOR UPDATE`, humanID); err != nil {
		return fmt.Errorf("database: lock human: %w", normalizeErr(err))
	}
	tag, err := tx.Exec(ctx, `UPDATE email_verification_tokens SET used_at = $3 WHERE id = $1 AND human_id = $2 AND used_at IS NULL AND expires_at > $3`, tokenID, humanID, now)
	if err != nil {
		return fmt.Errorf("database: consume email verification token: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrEmailVerificationTokenConsumed
	}
	if _, err := tx.Exec(ctx, `UPDATE humans SET email_verified_at = COALESCE(email_verified_at, $2), updated_at = $2 WHERE id = $1`, humanID, now); err != nil {
		return fmt.Errorf("database: mark email verified: %w", normalizeErr(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit verify email: %w", normalizeErr(err))
	}
	return nil
}
