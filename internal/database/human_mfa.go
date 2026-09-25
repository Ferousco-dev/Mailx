package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Storage for TOTP MFA and OAuth sign-in (migration 000032). Every
// single-use artifact here (challenge, backup code, OAuth state) is consumed
// with a guarded UPDATE/DELETE whose RowsAffected is checked, the same
// race-safety pattern as password_reset_tokens and org_invitations.

// ErrMFAConsumed means a challenge/backup code/TOTP step was already used,
// expired, or lost a concurrent consume race. Callers never distinguish.
var ErrMFAConsumed = errors.New("database: mfa artifact already used or expired")

// MFASecrets is a human's sealed TOTP material (never plaintext).
type MFASecrets struct {
	Enabled           bool
	SecretCiphertext  []byte
	SecretNonce       []byte
	PendingCiphertext []byte
	PendingNonce      []byte
}

// GetMFASecrets loads the sealed TOTP material for a human.
func (db *DB) GetMFASecrets(ctx context.Context, humanID string) (MFASecrets, error) {
	var m MFASecrets
	err := db.pool.QueryRow(ctx, `
		SELECT mfa_enabled, mfa_secret_ciphertext, mfa_secret_nonce, mfa_pending_secret_ciphertext, mfa_pending_secret_nonce
		FROM humans WHERE id = $1`, humanID,
	).Scan(&m.Enabled, &m.SecretCiphertext, &m.SecretNonce, &m.PendingCiphertext, &m.PendingNonce)
	if err != nil {
		return MFASecrets{}, normalizeErr(err)
	}
	return m, nil
}

// SetPendingMFA stores a freshly enrolled (unconfirmed) sealed secret and
// replaces the human's backup codes, but only while MFA is NOT enabled
// (re-enrolling an active factor requires disabling it first). Returns
// ErrConflict when MFA is already enabled.
func (db *DB) SetPendingMFA(ctx context.Context, humanID string, ciphertext, nonce []byte, backupCodeHashes []string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return normalizeErr(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE humans SET mfa_pending_secret_ciphertext = $2, mfa_pending_secret_nonce = $3, updated_at = now()
		WHERE id = $1 AND NOT mfa_enabled`, humanID, ciphertext, nonce)
	if err != nil {
		return normalizeErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `DELETE FROM mfa_backup_codes WHERE human_id = $1`, humanID); err != nil {
		return normalizeErr(err)
	}
	for _, h := range backupCodeHashes {
		id, err := newID()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO mfa_backup_codes (id, human_id, code_hash) VALUES ($1, $2, $3)`, id, humanID, h); err != nil {
			return normalizeErr(err)
		}
	}
	return normalizeErr(tx.Commit(ctx))
}

// ActivateMFA promotes the pending secret to active and enables MFA, recording
// step as the last used TOTP step (the confirming code cannot be replayed).
// The pending ciphertext is matched so a concurrent re-enroll cannot activate a
// secret the caller did not verify. Returns ErrMFAConsumed otherwise.
func (db *DB) ActivateMFA(ctx context.Context, humanID string, pendingCiphertext []byte, step int64) error {
	tag, err := db.pool.Exec(ctx, `
		UPDATE humans SET mfa_enabled = true,
			mfa_secret_ciphertext = mfa_pending_secret_ciphertext, mfa_secret_nonce = mfa_pending_secret_nonce,
			mfa_pending_secret_ciphertext = NULL, mfa_pending_secret_nonce = NULL,
			mfa_last_used_step = $3, updated_at = now()
		WHERE id = $1 AND NOT mfa_enabled AND mfa_pending_secret_ciphertext = $2`, humanID, pendingCiphertext, step)
	if err != nil {
		return normalizeErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrMFAConsumed
	}
	return nil
}

// DisableMFA clears all MFA state for a human: secrets, backup codes, and
// outstanding challenges.
func (db *DB) DisableMFA(ctx context.Context, humanID string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return normalizeErr(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stmts := []string{
		`UPDATE humans SET mfa_enabled = false, mfa_secret_ciphertext = NULL, mfa_secret_nonce = NULL,
			mfa_pending_secret_ciphertext = NULL, mfa_pending_secret_nonce = NULL, mfa_last_used_step = NULL, updated_at = now()
		 WHERE id = $1`,
		`DELETE FROM mfa_backup_codes WHERE human_id = $1`,
		`DELETE FROM mfa_challenges WHERE human_id = $1`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q, humanID); err != nil {
			return normalizeErr(err)
		}
	}
	return normalizeErr(tx.Commit(ctx))
}

// MFAChallenge is one issued login challenge row.
type MFAChallenge struct {
	ID             string
	HumanID        string
	ExpiresAt      time.Time
	UsedAt         *time.Time
	FailedAttempts int
}

// CreateMFAChallenge inserts a challenge for humanID.
func (db *DB) CreateMFAChallenge(ctx context.Context, humanID, tokenHash string, expiresAt time.Time) error {
	id, err := newID()
	if err != nil {
		return err
	}
	_, err = db.pool.Exec(ctx, `INSERT INTO mfa_challenges (id, human_id, token_hash, expires_at) VALUES ($1, $2, $3, $4)`,
		id, humanID, tokenHash, expiresAt)
	return normalizeErr(err)
}

// GetMFAChallengeByHash loads a challenge. ErrNotFound if absent.
func (db *DB) GetMFAChallengeByHash(ctx context.Context, tokenHash string) (MFAChallenge, error) {
	var c MFAChallenge
	err := db.pool.QueryRow(ctx, `SELECT id, human_id, expires_at, used_at, failed_attempts FROM mfa_challenges WHERE token_hash = $1`, tokenHash).
		Scan(&c.ID, &c.HumanID, &c.ExpiresAt, &c.UsedAt, &c.FailedAttempts)
	if err != nil {
		return MFAChallenge{}, normalizeErr(err)
	}
	return c, nil
}

// RecordMFAChallengeFailure counts a wrong code and burns the challenge once
// failed_attempts reaches maxAttempts.
func (db *DB) RecordMFAChallengeFailure(ctx context.Context, challengeID string, maxAttempts int, now time.Time) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE mfa_challenges SET failed_attempts = failed_attempts + 1,
			used_at = CASE WHEN failed_attempts + 1 >= $2 THEN COALESCE(used_at, $3) ELSE used_at END
		WHERE id = $1`, challengeID, maxAttempts, now)
	return normalizeErr(err)
}

// CompleteMFAChallenge atomically consumes the challenge AND the second factor
// that satisfied it: either a TOTP step (must be newer than the last accepted
// step, so a code cannot be replayed) or a backup code hash (must be unused).
// Exactly one of totpStep (non-nil) or backupCodeHash (non-empty) is used.
// Any guard failing rolls everything back and returns ErrMFAConsumed.
func (db *DB) CompleteMFAChallenge(ctx context.Context, challengeID, humanID string, now time.Time, totpStep *int64, backupCodeHash string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return normalizeErr(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE mfa_challenges SET used_at = $3 WHERE id = $1 AND human_id = $2 AND used_at IS NULL AND expires_at > $3`,
		challengeID, humanID, now)
	if err != nil {
		return normalizeErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrMFAConsumed
	}
	if totpStep != nil {
		tag, err = tx.Exec(ctx, `UPDATE humans SET mfa_last_used_step = $2 WHERE id = $1 AND mfa_enabled AND (mfa_last_used_step IS NULL OR mfa_last_used_step < $2)`,
			humanID, *totpStep)
	} else {
		tag, err = tx.Exec(ctx, `UPDATE mfa_backup_codes SET used_at = $3 WHERE human_id = $1 AND code_hash = $2 AND used_at IS NULL`,
			humanID, backupCodeHash, now)
	}
	if err != nil {
		return normalizeErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrMFAConsumed
	}
	return normalizeErr(tx.Commit(ctx))
}

// CreateOAuthState stores a hashed OAuth state value.
func (db *DB) CreateOAuthState(ctx context.Context, stateHash, provider string, expiresAt time.Time) error {
	_, err := db.pool.Exec(ctx, `INSERT INTO oauth_states (state_hash, provider, expires_at) VALUES ($1, $2, $3)`, stateHash, provider, expiresAt)
	return normalizeErr(err)
}

// ConsumeOAuthState deletes the state row (single use) and reports whether it
// existed for this provider and was unexpired. Expired rows are also swept.
func (db *DB) ConsumeOAuthState(ctx context.Context, stateHash, provider string, now time.Time) (bool, error) {
	tag, err := db.pool.Exec(ctx, `DELETE FROM oauth_states WHERE state_hash = $1 AND provider = $2 AND expires_at > $3`, stateHash, provider, now)
	if err != nil {
		return false, normalizeErr(err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM oauth_states WHERE expires_at <= $1`, now); err != nil {
		return false, normalizeErr(err)
	}
	return tag.RowsAffected() == 1, nil
}

// unusablePasswordHash is stored for accounts created via OAuth. It is not a
// valid bcrypt hash, so bcrypt.CompareHashAndPassword always fails and no
// password login is possible until the human sets one via password reset.
const unusablePasswordHash = "!oauth-no-password"

// ResolveOAuthHuman finds or creates the human for a provider identity, in one
// transaction: (1) an existing identity link wins; (2) otherwise a human with
// the same (case-insensitive, same normalization as CreateHuman) email is
// LINKED, not duplicated; (3) otherwise a new human is created with an
// unusable password. Callers must only pass a provider-verified email.
func (db *DB) ResolveOAuthHuman(ctx context.Context, provider, providerUserID, email, name string, avatarURL *string) (Human, bool, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return Human{}, false, normalizeErr(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var humanID string
	created := false
	err = tx.QueryRow(ctx, `SELECT human_id FROM human_oauth_identities WHERE provider = $1 AND provider_user_id = $2`, provider, providerUserID).Scan(&humanID)
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		err = tx.QueryRow(ctx, `SELECT id FROM humans WHERE normalized_email = $1`, normalizeEmail(email)).Scan(&humanID)
		if errors.Is(err, pgx.ErrNoRows) {
			id, idErr := newID()
			if idErr != nil {
				return Human{}, false, idErr
			}
			if _, err := tx.Exec(ctx, `INSERT INTO humans (id, name, normalized_email, email, password_hash, avatar_url) VALUES ($1, $2, $3, $4, $5, $6)`,
				id, name, normalizeEmail(email), email, unusablePasswordHash, avatarURL); err != nil {
				return Human{}, false, normalizeErr(err)
			}
			humanID, created = id, true
		} else if err != nil {
			return Human{}, false, normalizeErr(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO human_oauth_identities (provider, provider_user_id, human_id, email) VALUES ($1, $2, $3, $4)`,
			provider, providerUserID, humanID, email); err != nil {
			return Human{}, false, normalizeErr(err)
		}
	default:
		return Human{}, false, normalizeErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Human{}, false, fmt.Errorf("database: commit oauth resolve: %w", normalizeErr(err))
	}
	h, err := db.GetHuman(ctx, humanID)
	return h, created, err
}
