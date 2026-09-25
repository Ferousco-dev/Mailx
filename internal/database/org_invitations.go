package database

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// OrgInvitation is one issued organization-invitation row (migration
// 000030). Deliberately its own table, not a repurposed password_reset_tokens
// row — an invite authorizes joining a specific tenant, not authenticating
// an existing account, and carries fields (tenant_id, invited_by, email)
// that have no analog there.
type OrgInvitation struct {
	ID              string
	TenantID        string
	InvitedBy       string
	NormalizedEmail string
	RawEmail        string
	TokenHash       string
	ExpiresAt       time.Time
	AcceptedAt      *time.Time
	CreatedAt       time.Time
}

// IsTenantOwner reports whether humanID is an 'owner' member of tenantID —
// the only role permitted to send an org invitation (operator decision:
// owner-only, no broader RBAC yet — see DEC-205's deferral of a full role
// matrix).
func (db *DB) IsTenantOwner(ctx context.Context, tenantID, humanID string) (bool, error) {
	var role string
	err := db.pool.QueryRow(ctx,
		`SELECT role FROM tenant_members WHERE tenant_id = $1 AND human_id = $2`, tenantID, humanID,
	).Scan(&role)
	if err != nil {
		if errors.Is(normalizeErr(err), ErrNotFound) {
			return false, nil
		}
		return false, normalizeErr(err)
	}
	return role == "owner", nil
}

// CreateOrgInvitation inserts a new invitation row.
func (db *DB) CreateOrgInvitation(ctx context.Context, tenantID, invitedBy, email, tokenHash string, expiresAt time.Time) (OrgInvitation, error) {
	id, err := newID()
	if err != nil {
		return OrgInvitation{}, err
	}
	var inv OrgInvitation
	err = db.pool.QueryRow(ctx, `
		INSERT INTO org_invitations (id, tenant_id, invited_by, normalized_email, raw_email, token_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, tenant_id, invited_by, normalized_email, raw_email, token_hash, expires_at, accepted_at, created_at`,
		id, tenantID, invitedBy, normalizeEmail(email), email, tokenHash, expiresAt,
	).Scan(&inv.ID, &inv.TenantID, &inv.InvitedBy, &inv.NormalizedEmail, &inv.RawEmail, &inv.TokenHash, &inv.ExpiresAt, &inv.AcceptedAt, &inv.CreatedAt)
	if err != nil {
		return OrgInvitation{}, normalizeErr(err)
	}
	return inv, nil
}

// GetOrgInvitationByHash loads an invitation row by its hash. Returns
// ErrNotFound if absent (regardless of accepted/expired — callers check
// those fields themselves, same convention as GetPasswordResetTokenByHash).
func (db *DB) GetOrgInvitationByHash(ctx context.Context, tokenHash string) (OrgInvitation, error) {
	var inv OrgInvitation
	err := db.pool.QueryRow(ctx, `
		SELECT id, tenant_id, invited_by, normalized_email, raw_email, token_hash, expires_at, accepted_at, created_at
		FROM org_invitations WHERE token_hash = $1`, tokenHash,
	).Scan(&inv.ID, &inv.TenantID, &inv.InvitedBy, &inv.NormalizedEmail, &inv.RawEmail, &inv.TokenHash, &inv.ExpiresAt, &inv.AcceptedAt, &inv.CreatedAt)
	if err != nil {
		return OrgInvitation{}, normalizeErr(err)
	}
	return inv, nil
}

// ErrOrgInvitationConsumed means this exact invitation row was already
// marked accepted by a concurrent call — same race-safety pattern as
// ErrPasswordResetTokenConsumed.
var ErrOrgInvitationConsumed = errors.New("database: org invitation already accepted")

// AcceptOrgInvitationForExistingHuman atomically: (1) marks the invitation
// accepted, but ONLY if not already accepted (RowsAffected-checked, same
// pattern as ResetPassword), and (2) inserts the tenant_members row —
// ON CONFLICT DO NOTHING, since the invitee may already belong to the
// tenant (e.g. re-clicking an old link after already joining some other
// way must not error). Used when the invite is accepted by an already
// logged-in human whose account email matches the invitation.
func (db *DB) AcceptOrgInvitationForExistingHuman(ctx context.Context, invitationID, tenantID, humanID string, now time.Time) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin accept invitation: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `UPDATE org_invitations SET accepted_at = $2 WHERE id = $1 AND accepted_at IS NULL`, invitationID, now)
	if err != nil {
		return fmt.Errorf("database: consume org invitation: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrOrgInvitationConsumed
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO tenant_members (tenant_id, human_id, role) VALUES ($1, $2, 'member') ON CONFLICT DO NOTHING`,
		tenantID, humanID,
	); err != nil {
		return fmt.Errorf("database: insert tenant member: %w", normalizeErr(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit accept invitation: %w", normalizeErr(err))
	}
	return nil
}

// AcceptOrgInvitationWithSignup atomically: (1) marks the invitation
// accepted (same RowsAffected guard as AcceptOrgInvitationForExistingHuman),
// (2) creates the new human account, and (3) inserts the tenant_members
// row — all three or none, so a partial application (account created but
// never joined, or vice versa) is never observable. Used when the invitee
// has no MailX account yet: the invite link itself carries them through
// signup, so email is always the invitation's own (never client-supplied,
// to prevent an invite token for one address minting an account under
// another).
func (db *DB) AcceptOrgInvitationWithSignup(ctx context.Context, invitationID, tenantID, name, email, passwordHash string, now time.Time) (Human, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return Human{}, fmt.Errorf("database: begin accept invitation with signup: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `UPDATE org_invitations SET accepted_at = $2 WHERE id = $1 AND accepted_at IS NULL`, invitationID, now)
	if err != nil {
		return Human{}, fmt.Errorf("database: consume org invitation: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return Human{}, ErrOrgInvitationConsumed
	}

	humanID, err := newID()
	if err != nil {
		return Human{}, err
	}
	var h Human
	err = tx.QueryRow(ctx, `
		INSERT INTO humans (id, name, normalized_email, email, password_hash)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, name, email, password_hash, role, created_at, updated_at`,
		humanID, name, normalizeEmail(email), email, passwordHash,
	).Scan(&h.ID, &h.Name, &h.Email, &h.PasswordHash, &h.Role, &h.CreatedAt, &h.UpdatedAt)
	if err != nil {
		return Human{}, normalizeErr(err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO tenant_members (tenant_id, human_id, role) VALUES ($1, $2, 'member')`,
		tenantID, h.ID,
	); err != nil {
		return Human{}, normalizeErr(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Human{}, fmt.Errorf("database: commit accept invitation with signup: %w", normalizeErr(err))
	}
	return h, nil
}
