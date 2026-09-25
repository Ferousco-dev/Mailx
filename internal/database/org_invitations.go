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

// CreateOrgInvitation inserts a new invitation row. It deliberately does
// NOT invalidate any other pending invitation for the same address here -
// see SupersedeOtherPendingOrgInvitations, called only after the new
// invitation's email has actually been sent (Greptile P1, PR #23: an
// earlier version invalidated the old link and committed the new one
// before attempting delivery, so a failed send left the invitee with
// NEITHER a working old link nor a delivered new one).
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

// SupersedeOtherPendingOrgInvitations invalidates every OTHER still-pending
// (unaccepted, unexpired) invitation for the same (tenant, email) that
// ORDERS STRICTLY BEFORE the keeper - called only once the keeper
// invitation's email has been confirmed sent (see CreateOrgInvitation's
// doc). Ordering by creation time, not simply "id != keepID", matters for
// correctness under concurrency: two invitations sent to the same address
// at nearly the same instant would otherwise each try to supersede the
// OTHER after both had already been sent, and whichever update ran last
// would win - expiring the very link that had just been delivered, so
// BOTH emailed links end up dead even though both requests reported
// success (Greptile P1, PR #23). Superseding only strictly-older rows
// makes this commutative: the newest invitation always survives no matter
// which request's UPDATE happens to run last. created_at alone is not a
// total order - two rows can share a timestamp (Postgres timestamptz is
// microsecond-precision, and two concurrent inserts can land in the same
// microsecond) - so id breaks the tie: id is a crypto-random newID(), so
// for two invitations with equal created_at, both this call and its
// concurrent sibling used to see itself as the older one and would leave
// both alive; comparing "(created_at, id) < (keepCreatedAt, keepID)"
// forces a single, consistent winner regardless of which row queries
// first (Greptile P2, PR #23).
func (db *DB) SupersedeOtherPendingOrgInvitations(ctx context.Context, tenantID, email, keepID string, keepCreatedAt, now time.Time) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE org_invitations SET expires_at = $5 WHERE tenant_id = $1 AND normalized_email = $2 AND (created_at, id) < ($4, $3) AND accepted_at IS NULL AND expires_at > $5`,
		tenantID, normalizeEmail(email), keepID, keepCreatedAt, now,
	)
	if err != nil {
		return fmt.Errorf("database: supersede prior pending invitations: %w", normalizeErr(err))
	}
	return nil
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

// ErrOrgInvitationConsumed covers both ways the accept UPDATE can affect
// zero rows: the invitation was already accepted by a concurrent call, OR
// it passed its expires_at between the service layer's own expiry check
// and this statement actually running (a slow request, GC pause, or lock
// wait can carry a borderline-valid request past the 5-hour deadline) -
// the WHERE clause rechecks expiry here for exactly that reason, so
// nothing can be granted membership after the window closes (Greptile P1,
// PR #23). Same race-safety pattern as ErrPasswordResetTokenConsumed;
// deliberately not distinguished from the "already accepted" case, since
// the caller collapses both into the same generic ErrOrgInvitationInvalid.
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

	tag, err := tx.Exec(ctx, `UPDATE org_invitations SET accepted_at = $2 WHERE id = $1 AND accepted_at IS NULL AND expires_at > $2`, invitationID, now)
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

	tag, err := tx.Exec(ctx, `UPDATE org_invitations SET accepted_at = $2 WHERE id = $1 AND accepted_at IS NULL AND expires_at > $2`, invitationID, now)
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
