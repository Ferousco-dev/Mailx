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

// CreateEmailVerificationToken locks the humans row (serializing concurrent
// issues for the same account and providing a consistent point to check
// "not already verified"), then inserts a fresh token. It deliberately does
// NOT invalidate any other pending token here - see
// SupersedeOtherEmailVerificationTokens, called only after this token's
// email has actually been sent. An earlier version invalidated the old
// token before attempting delivery, so a failed send left the account with
// NEITHER a working old link nor a delivered new one (the exact bug class
// DEC-218 fixed for org invitations - caught here before merge, not by a
// live incident).
//
// created_at is set via clock_timestamp(), not the column's now() default:
// now()/transaction_timestamp() is fixed at this transaction's BEGIN, which
// happens BEFORE the FOR UPDATE lock wait above - a transaction that
// waited on the lock would otherwise still capture an earlier timestamp
// than one that acquired the lock and committed first, which would break
// SupersedeOtherEmailVerificationTokens' created_at-ordering guarantee
// exactly the way DEC-226 found for org invitations. clock_timestamp()
// reflects the actual moment this INSERT runs, i.e. after the lock is held.
func (db *DB) CreateEmailVerificationToken(ctx context.Context, humanID, tokenHash string, expiresAt time.Time) (EmailVerificationToken, error) {
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
	var t EmailVerificationToken
	err = tx.QueryRow(ctx, `
		INSERT INTO email_verification_tokens (id, human_id, token_hash, expires_at, created_at)
		VALUES ($1, $2, $3, $4, clock_timestamp())
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

// SupersedeOtherEmailVerificationTokens invalidates every OTHER still-valid
// verification token for humanID that ORDERS STRICTLY BEFORE keepTokenID -
// called only once the keeper token's email has been confirmed sent (see
// CreateEmailVerificationToken's doc). Ordering by (created_at, id), not
// simply "id != keep", matters under concurrency for the exact reason
// DEC-219 found for org invitations: two resends completing their sends at
// nearly the same instant would otherwise each try to supersede the OTHER
// after both had already been sent, and whichever UPDATE ran last would
// win - invalidating the link that had just been delivered, so BOTH
// emailed links could end up dead even though both requests reported
// success. Superseding only strictly-older rows makes this commutative:
// the newest token always survives no matter which request's UPDATE runs
// last. created_at alone is not a total order (two rows can share a
// timestamp), so id breaks the tie, matching DEC-226's fix.
func (db *DB) SupersedeOtherEmailVerificationTokens(ctx context.Context, humanID, keepTokenID string, keepCreatedAt, now time.Time) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE email_verification_tokens SET used_at = $4 WHERE human_id = $1 AND (created_at, id) < ($3, $2) AND used_at IS NULL`,
		humanID, keepTokenID, keepCreatedAt, now,
	)
	if err != nil {
		return fmt.Errorf("database: supersede email verification tokens: %w", normalizeErr(err))
	}
	return nil
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
