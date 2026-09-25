package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Human is a platform user account — distinct from Tenant (an org) and
// APIKey (a machine credential). See migration 000025's doc for why.
type Human struct {
	ID           string
	Name         string
	Email        string
	PasswordHash string
	Role         string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastLoginAt  *time.Time
}

// RefreshToken is one issued refresh token row (see migration 000025).
type RefreshToken struct {
	ID        string
	HumanID   string
	TokenHash string
	ExpiresAt time.Time
	CreatedAt time.Time
	RevokedAt *time.Time
}

// TenantMember is one row of the tenants<->humans membership table.
type TenantMember struct {
	TenantID  string
	HumanID   string
	Role      string
	CreatedAt time.Time
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// CreateHuman inserts a new human account. Returns ErrConflict if the
// (case-insensitive) email is already taken.
func (db *DB) CreateHuman(ctx context.Context, name, email, passwordHash string) (Human, error) {
	id, err := newID()
	if err != nil {
		return Human{}, err
	}
	var h Human
	err = db.pool.QueryRow(ctx, `
		INSERT INTO humans (id, name, normalized_email, email, password_hash)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, name, email, password_hash, role, created_at, updated_at`,
		id, name, normalizeEmail(email), email, passwordHash,
	).Scan(&h.ID, &h.Name, &h.Email, &h.PasswordHash, &h.Role, &h.CreatedAt, &h.UpdatedAt)
	if err != nil {
		return Human{}, normalizeErr(err)
	}
	return h, nil
}

// GetHumanByEmail loads a human by case-insensitive email. Returns
// ErrNotFound if absent.
func (db *DB) GetHumanByEmail(ctx context.Context, email string) (Human, error) {
	var h Human
	err := db.pool.QueryRow(ctx, `
		SELECT id, name, email, password_hash, role, created_at, updated_at, last_login_at
		FROM humans WHERE normalized_email = $1`,
		normalizeEmail(email),
	).Scan(&h.ID, &h.Name, &h.Email, &h.PasswordHash, &h.Role, &h.CreatedAt, &h.UpdatedAt, &h.LastLoginAt)
	if err != nil {
		return Human{}, normalizeErr(err)
	}
	return h, nil
}

// GetHuman loads a human by ID.
func (db *DB) GetHuman(ctx context.Context, id string) (Human, error) {
	var h Human
	err := db.pool.QueryRow(ctx, `
		SELECT id, name, email, password_hash, role, created_at, updated_at, last_login_at
		FROM humans WHERE id = $1`, id,
	).Scan(&h.ID, &h.Name, &h.Email, &h.PasswordHash, &h.Role, &h.CreatedAt, &h.UpdatedAt, &h.LastLoginAt)
	if err != nil {
		return Human{}, normalizeErr(err)
	}
	return h, nil
}

// TouchHumanLogin records a successful login: sets last_login_at and bumps
// updated_at, but ONLY if at is strictly newer than the currently stored
// last_login_at (or there isn't one yet) - two logins for the same account
// completing out of wall-clock order (a slower request finishing after a
// faster, later one) must not walk the stored timestamp backwards. Prefer
// TouchLoginAndCreateRefreshToken for an actual Login flow - this
// standalone form exists for callers that only need the timestamp, not a
// session.
func (db *DB) TouchHumanLogin(ctx context.Context, humanID string, at time.Time) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE humans SET last_login_at = $2, updated_at = $2 WHERE id = $1 AND (last_login_at IS NULL OR last_login_at < $2)`,
		humanID, at)
	if err != nil {
		return normalizeErr(err)
	}
	return nil
}

// TouchLoginAndCreateRefreshToken records a login AND issues its refresh
// token in one transaction: either both happen or neither does. Splitting
// these into two separate statements (as an earlier version did) meant a
// refresh-token insert failure after a successful login-timestamp update
// left the account permanently recording a "successful" login for an
// attempt that actually failed and returned no session to the caller.
// tokenHash/expiresAt are precomputed by the caller (internal/humanauth
// owns token generation, not this package). See TouchHumanLogin's doc for
// the monotonic-timestamp guard, applied identically here.
func (db *DB) TouchLoginAndCreateRefreshToken(ctx context.Context, humanID string, loginAt time.Time, tokenHash string, expiresAt time.Time) (RefreshToken, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return RefreshToken{}, fmt.Errorf("database: begin login: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`UPDATE humans SET last_login_at = $2, updated_at = $2 WHERE id = $1 AND (last_login_at IS NULL OR last_login_at < $2)`,
		humanID, loginAt); err != nil {
		return RefreshToken{}, fmt.Errorf("database: touch human login: %w", normalizeErr(err))
	}

	id, err := newID()
	if err != nil {
		return RefreshToken{}, err
	}
	var t RefreshToken
	err = tx.QueryRow(ctx, `
		INSERT INTO refresh_tokens (id, human_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING id, human_id, token_hash, expires_at, created_at, revoked_at`,
		id, humanID, tokenHash, expiresAt,
	).Scan(&t.ID, &t.HumanID, &t.TokenHash, &t.ExpiresAt, &t.CreatedAt, &t.RevokedAt)
	if err != nil {
		return RefreshToken{}, fmt.Errorf("database: create refresh token: %w", normalizeErr(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return RefreshToken{}, fmt.Errorf("database: commit login: %w", normalizeErr(err))
	}
	return t, nil
}

// CreateRefreshToken inserts a new refresh token row.
func (db *DB) CreateRefreshToken(ctx context.Context, humanID, tokenHash string, expiresAt time.Time) (RefreshToken, error) {
	id, err := newID()
	if err != nil {
		return RefreshToken{}, err
	}
	var t RefreshToken
	err = db.pool.QueryRow(ctx, `
		INSERT INTO refresh_tokens (id, human_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING id, human_id, token_hash, expires_at, created_at, revoked_at`,
		id, humanID, tokenHash, expiresAt,
	).Scan(&t.ID, &t.HumanID, &t.TokenHash, &t.ExpiresAt, &t.CreatedAt, &t.RevokedAt)
	if err != nil {
		return RefreshToken{}, normalizeErr(err)
	}
	return t, nil
}

// GetRefreshTokenByHash loads a refresh token row by its hash. Returns
// ErrNotFound if absent (regardless of whether it's revoked/expired —
// callers check those fields themselves).
func (db *DB) GetRefreshTokenByHash(ctx context.Context, tokenHash string) (RefreshToken, error) {
	var t RefreshToken
	err := db.pool.QueryRow(ctx, `
		SELECT id, human_id, token_hash, expires_at, created_at, revoked_at
		FROM refresh_tokens WHERE token_hash = $1`, tokenHash,
	).Scan(&t.ID, &t.HumanID, &t.TokenHash, &t.ExpiresAt, &t.CreatedAt, &t.RevokedAt)
	if err != nil {
		return RefreshToken{}, normalizeErr(err)
	}
	return t, nil
}

// RevokeRefreshToken marks one refresh token row revoked, but ONLY if it
// was not already revoked - the boolean return is load-bearing, not
// informational: two concurrent Refresh calls for the same token both
// pass the caller's earlier not-revoked check before either one gets
// here, so whichever UPDATE actually flips revoked_at (RowsAffected=1,
// returns true) is the sole winner of that rotation; the loser
// (RowsAffected=0, returns false) MUST NOT mint a new session, or a
// single-use refresh token could hand out two valid sessions from one
// concurrent race.
func (db *DB) RevokeRefreshToken(ctx context.Context, id string, now time.Time) (bool, error) {
	tag, err := db.pool.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, id, now)
	if err != nil {
		return false, normalizeErr(err)
	}
	return tag.RowsAffected() > 0, nil
}

// ErrRefreshTokenRotationLost signals that the old token was already
// revoked by someone else (a concurrent Refresh, a Logout, or a
// password-reset revoke-all) before this call's own revoke ran.
var ErrRefreshTokenRotationLost = errors.New("database: refresh token already revoked")

// RotateRefreshToken revokes oldID and inserts its replacement in a single
// transaction. This must be one transaction, not two separate calls: if the
// revoke and the insert were independent statements, a concurrent
// ResetPassword's "revoke every active token" update could run in the gap
// between them and never see the new token, since it didn't exist yet -
// leaving a session alive after a reset that was supposed to end all of
// them. Keeping both statements in one transaction removes that gap: either
// this whole rotation lands before the reset's revoke-all (which then also
// revokes the new token), or the reset's revoke-all lands first (which
// revokes the old token, so this call's own revoke affects zero rows and
// returns ErrRefreshTokenRotationLost instead of minting a session).
func (db *DB) RotateRefreshToken(ctx context.Context, oldID, humanID, tokenHash string, now, expiresAt time.Time) (RefreshToken, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return RefreshToken{}, fmt.Errorf("database: begin rotate refresh token: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, oldID, now)
	if err != nil {
		return RefreshToken{}, fmt.Errorf("database: revoke old refresh token: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return RefreshToken{}, ErrRefreshTokenRotationLost
	}

	id, err := newID()
	if err != nil {
		return RefreshToken{}, err
	}
	var t RefreshToken
	err = tx.QueryRow(ctx, `
		INSERT INTO refresh_tokens (id, human_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING id, human_id, token_hash, expires_at, created_at, revoked_at`,
		id, humanID, tokenHash, expiresAt,
	).Scan(&t.ID, &t.HumanID, &t.TokenHash, &t.ExpiresAt, &t.CreatedAt, &t.RevokedAt)
	if err != nil {
		return RefreshToken{}, fmt.Errorf("database: insert rotated refresh token: %w", normalizeErr(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return RefreshToken{}, fmt.Errorf("database: commit rotate refresh token: %w", normalizeErr(err))
	}
	return t, nil
}

// RevokeAllRefreshTokensForHuman revokes every active refresh token for a
// human — the session-wide compromise response to a detected reuse of an
// already-rotated token.
func (db *DB) RevokeAllRefreshTokensForHuman(ctx context.Context, humanID string, now time.Time) error {
	_, err := db.pool.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = $2 WHERE human_id = $1 AND revoked_at IS NULL`, humanID, now)
	if err != nil {
		return normalizeErr(err)
	}
	return nil
}

// CreateOrganization creates a tenant and inserts the creating human as
// its 'owner' member, atomically: either both rows exist or neither does.
func (db *DB) CreateOrganization(ctx context.Context, humanID, name string) (Tenant, error) {
	if strings.TrimSpace(name) == "" {
		return Tenant{}, fmt.Errorf("database: tenant name is empty")
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return Tenant{}, fmt.Errorf("database: begin create organization: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tenantID, err := newID()
	if err != nil {
		return Tenant{}, err
	}
	var t Tenant
	err = tx.QueryRow(ctx,
		`INSERT INTO tenants (id, name) VALUES ($1, $2) RETURNING id, name, created_at`,
		tenantID, name,
	).Scan(&t.ID, &t.Name, &t.CreatedAt)
	if err != nil {
		return Tenant{}, normalizeErr(err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO tenant_members (tenant_id, human_id, role) VALUES ($1, $2, 'owner')`,
		tenantID, humanID,
	); err != nil {
		return Tenant{}, normalizeErr(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Tenant{}, fmt.Errorf("database: commit create organization: %w", normalizeErr(err))
	}
	return t, nil
}

// ListOrganizationsForHuman returns tenants the given human is a member of.
func (db *DB) ListOrganizationsForHuman(ctx context.Context, humanID string) ([]Tenant, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT t.id, t.name, t.created_at
		FROM tenants t
		JOIN tenant_members m ON m.tenant_id = t.id
		WHERE m.human_id = $1
		ORDER BY t.created_at DESC, t.id DESC`, humanID)
	if err != nil {
		return nil, normalizeErr(err)
	}
	defer rows.Close()

	var out []Tenant
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
			return nil, normalizeErr(err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, normalizeErr(err)
	}
	return out, nil
}

// PasswordResetToken is one issued password-reset token row (migration
// 000029). Deliberately a separate table from refresh_tokens - see that
// migration's doc.
type PasswordResetToken struct {
	ID        string
	HumanID   string
	TokenHash string
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
}

// CreatePasswordResetToken inserts a new password-reset token row.
func (db *DB) CreatePasswordResetToken(ctx context.Context, humanID, tokenHash string, expiresAt time.Time) (PasswordResetToken, error) {
	id, err := newID()
	if err != nil {
		return PasswordResetToken{}, err
	}
	var t PasswordResetToken
	err = db.pool.QueryRow(ctx, `
		INSERT INTO password_reset_tokens (id, human_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING id, human_id, token_hash, expires_at, used_at, created_at`,
		id, humanID, tokenHash, expiresAt,
	).Scan(&t.ID, &t.HumanID, &t.TokenHash, &t.ExpiresAt, &t.UsedAt, &t.CreatedAt)
	if err != nil {
		return PasswordResetToken{}, normalizeErr(err)
	}
	return t, nil
}

// GetPasswordResetTokenByHash loads a password-reset token row by its
// hash. Returns ErrNotFound if absent (regardless of used/expired -
// callers check those fields themselves, same convention as
// GetRefreshTokenByHash).
func (db *DB) GetPasswordResetTokenByHash(ctx context.Context, tokenHash string) (PasswordResetToken, error) {
	var t PasswordResetToken
	err := db.pool.QueryRow(ctx, `
		SELECT id, human_id, token_hash, expires_at, used_at, created_at
		FROM password_reset_tokens WHERE token_hash = $1`, tokenHash,
	).Scan(&t.ID, &t.HumanID, &t.TokenHash, &t.ExpiresAt, &t.UsedAt, &t.CreatedAt)
	if err != nil {
		return PasswordResetToken{}, normalizeErr(err)
	}
	return t, nil
}

// ErrPasswordResetTokenConsumed means this exact token row was already
// marked used by a concurrent call - see ResetPassword's doc.
var ErrPasswordResetTokenConsumed = errors.New("database: password reset token already used")

// ResetPassword atomically: (1) marks tokenID used, but ONLY if it is not
// ALREADY used (RowsAffected-checked, same race-safety pattern as
// RevokeRefreshToken - two concurrent reset-password calls with the SAME
// valid token must not both succeed), (2) updates the human's password
// hash, and (3) revokes every active refresh token for that human -
// changing a password must end every other existing session, the same
// security posture as detecting refresh-token reuse (DEC-208). All three
// happen in one transaction: a partial application (password changed but
// old sessions still valid, or vice versa) is never observable.
func (db *DB) ResetPassword(ctx context.Context, tokenID, humanID, newPasswordHash string, now time.Time) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin reset password: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `UPDATE password_reset_tokens SET used_at = $2 WHERE id = $1 AND used_at IS NULL`, tokenID, now)
	if err != nil {
		return fmt.Errorf("database: consume password reset token: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrPasswordResetTokenConsumed
	}

	if _, err := tx.Exec(ctx, `UPDATE humans SET password_hash = $2, updated_at = $3 WHERE id = $1`, humanID, newPasswordHash, now); err != nil {
		return fmt.Errorf("database: update password: %w", normalizeErr(err))
	}
	if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = $2 WHERE human_id = $1 AND revoked_at IS NULL`, humanID, now); err != nil {
		return fmt.Errorf("database: revoke sessions on password reset: %w", normalizeErr(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit reset password: %w", normalizeErr(err))
	}
	return nil
}
