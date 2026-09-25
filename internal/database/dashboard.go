package database

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Dashboard (v0.47 phase 3a) org/member/profile data access. Membership and
// ownership reads reuse IsTenantMember / IsTenantOwner; this file only holds
// the writes and list queries the dashboard needs (DEC-228..230).

var (
	// ErrNotTenantOwner: the acting human is not (or, under the tenant lock,
	// is no longer) an owner of the tenant.
	ErrNotTenantOwner = errors.New("database: not a tenant owner")
	// ErrCannotRemoveSelf: an owner tried to remove their own membership;
	// leaving/transferring ownership is a separate, not-yet-built flow.
	ErrCannotRemoveSelf = errors.New("database: cannot remove yourself")
	// ErrLastOwner: the removal would leave the tenant with zero owners.
	ErrLastOwner = errors.New("database: cannot remove the last owner")
)

// UpdateHumanProfile sets name and/or avatar_url (nil = leave unchanged; an
// empty avatarURL clears it). Email is deliberately not updatable here.
func (db *DB) UpdateHumanProfile(ctx context.Context, humanID string, name, avatarURL *string) error {
	tag, err := db.pool.Exec(ctx, `
		UPDATE humans SET
			name = COALESCE($2, name),
			avatar_url = CASE WHEN $3::text IS NULL THEN avatar_url ELSE NULLIF($3::text, '') END,
			updated_at = now()
		WHERE id = $1`, humanID, name, avatarURL)
	if err != nil {
		return fmt.Errorf("database: update human profile: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateOrganization sets name and/or logo_url with the same nil/empty
// semantics as UpdateHumanProfile. Authorization is the caller's job.
func (db *DB) UpdateOrganization(ctx context.Context, tenantID string, name, logoURL *string) error {
	tag, err := db.pool.Exec(ctx, `
		UPDATE tenants SET
			name = COALESCE($2, name),
			logo_url = CASE WHEN $3::text IS NULL THEN logo_url ELSE NULLIF($3::text, '') END
		WHERE id = $1`, tenantID, name, logoURL)
	if err != nil {
		return fmt.Errorf("database: update organization: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// OrgMember is one member of a tenant joined with the human's profile.
type OrgMember struct {
	HumanID   string
	Name      string
	Email     string
	AvatarURL *string
	Role      string
	JoinedAt  time.Time
}

// ListTenantMembers returns every member of tenantID, oldest first.
func (db *DB) ListTenantMembers(ctx context.Context, tenantID string) ([]OrgMember, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT h.id, h.name, h.email, h.avatar_url, m.role, m.created_at
		FROM tenant_members m JOIN humans h ON h.id = m.human_id
		WHERE m.tenant_id = $1
		ORDER BY m.created_at, h.id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("database: list members: %w", normalizeErr(err))
	}
	defer rows.Close()
	out := []OrgMember{}
	for rows.Next() {
		var m OrgMember
		if err := rows.Scan(&m.HumanID, &m.Name, &m.Email, &m.AvatarURL, &m.Role, &m.JoinedAt); err != nil {
			return nil, normalizeErr(err)
		}
		out = append(out, m)
	}
	return out, normalizeErr(rows.Err())
}

// RemoveTenantMember removes targetID from tenantID on behalf of actorID.
// It locks the tenant row (SELECT ... FOR UPDATE, the lockMemberCap
// pattern) so every removal for one tenant serializes, then re-checks
// under that lock that the actor is still an owner and that at least one
// owner remains afterwards. Two owners removing each other concurrently
// therefore cannot both succeed: the second one finds its actor gone
// (ErrNotTenantOwner). ErrNotFound if target is not a member.
func (db *DB) RemoveTenantMember(ctx context.Context, tenantID, actorID, targetID string) error {
	if actorID == targetID {
		return ErrCannotRemoveSelf
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin remove member: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM tenants WHERE id = $1 FOR UPDATE`, tenantID).Scan(&one); err != nil {
		return fmt.Errorf("database: lock tenant for member removal: %w", normalizeErr(err))
	}
	var actorOwner bool
	var targetRole *string
	var owners int
	if err := tx.QueryRow(ctx, `
		SELECT
			coalesce(bool_or(human_id = $2 AND role = 'owner'), false),
			max(role) FILTER (WHERE human_id = $3),
			count(*) FILTER (WHERE role = 'owner')
		FROM tenant_members WHERE tenant_id = $1`, tenantID, actorID, targetID,
	).Scan(&actorOwner, &targetRole, &owners); err != nil {
		return fmt.Errorf("database: read members for removal: %w", normalizeErr(err))
	}
	if !actorOwner {
		return ErrNotTenantOwner
	}
	if targetRole == nil {
		return ErrNotFound
	}
	if *targetRole == "owner" && owners <= 1 {
		return ErrLastOwner // unreachable while actor is a distinct owner; kept as the invariant's last line
	}
	if _, err := tx.Exec(ctx, `DELETE FROM tenant_members WHERE tenant_id = $1 AND human_id = $2`, tenantID, targetID); err != nil {
		return fmt.Errorf("database: delete member: %w", normalizeErr(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit remove member: %w", normalizeErr(err))
	}
	return nil
}

// ListPendingOrgInvitations returns tenantID's unaccepted, unexpired
// invitations as of now, newest first.
func (db *DB) ListPendingOrgInvitations(ctx context.Context, tenantID string, now time.Time) ([]OrgInvitation, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, tenant_id, invited_by, normalized_email, raw_email, token_hash, expires_at, accepted_at, created_at
		FROM org_invitations
		WHERE tenant_id = $1 AND accepted_at IS NULL AND expires_at > $2
		ORDER BY created_at DESC, id DESC`, tenantID, now)
	if err != nil {
		return nil, fmt.Errorf("database: list pending invitations: %w", normalizeErr(err))
	}
	defer rows.Close()
	out := []OrgInvitation{}
	for rows.Next() {
		var inv OrgInvitation
		if err := rows.Scan(&inv.ID, &inv.TenantID, &inv.InvitedBy, &inv.NormalizedEmail, &inv.RawEmail, &inv.TokenHash, &inv.ExpiresAt, &inv.AcceptedAt, &inv.CreatedAt); err != nil {
			return nil, normalizeErr(err)
		}
		out = append(out, inv)
	}
	return out, normalizeErr(rows.Err())
}

// RevokeOrgInvitation invalidates a pending invitation by setting
// expires_at = now, the same mechanism SupersedeOtherPendingOrgInvitations
// uses. Idempotent: an already expired/accepted invitation is a no-op.
// ErrNotFound only if no invitation with that ID belongs to tenantID.
func (db *DB) RevokeOrgInvitation(ctx context.Context, tenantID, invitationID string, now time.Time) error {
	var exists bool
	err := db.pool.QueryRow(ctx, `
		WITH upd AS (
			UPDATE org_invitations SET expires_at = $3
			WHERE id = $2 AND tenant_id = $1 AND accepted_at IS NULL AND expires_at > $3
		)
		SELECT EXISTS (SELECT 1 FROM org_invitations WHERE id = $2 AND tenant_id = $1)`,
		tenantID, invitationID, now,
	).Scan(&exists)
	if err != nil {
		return fmt.Errorf("database: revoke invitation: %w", normalizeErr(err))
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}
