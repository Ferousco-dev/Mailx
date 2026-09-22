package database

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Audience is a tenant-owned named group of Contacts (membership only — no
// sending). See migration 000018 for how tenant integrity is enforced.
type Audience struct {
	ID        string
	TenantID  string
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type NewAudience struct {
	TenantID string
	Name     string
}

type AudienceCursor struct {
	CreatedAt time.Time
	ID        string
}

// MembershipCursor paginates ListAudienceMembers, keyed by the membership
// row's own created_at/contact_id (not the contact's).
type MembershipCursor struct {
	CreatedAt time.Time
	ContactID string
}

const audienceColumns = `id, tenant_id, name, created_at, updated_at`

func scanAudience(row rowScanner) (Audience, error) {
	var a Audience
	err := row.Scan(&a.ID, &a.TenantID, &a.Name, &a.CreatedAt, &a.UpdatedAt)
	return a, normalizeErr(err)
}

func (db *DB) CreateAudience(ctx context.Context, in NewAudience) (Audience, error) {
	id, err := newID()
	if err != nil {
		return Audience{}, err
	}
	a, err := scanAudience(db.pool.QueryRow(ctx, `
		INSERT INTO audiences (id, tenant_id, name) VALUES ($1,$2,$3)
		RETURNING `+audienceColumns, id, in.TenantID, in.Name))
	if err != nil {
		return Audience{}, fmt.Errorf("database: create audience: %w", err)
	}
	return a, nil
}

func (db *DB) GetAudience(ctx context.Context, tenantID, id string) (Audience, error) {
	a, err := scanAudience(db.pool.QueryRow(ctx,
		`SELECT `+audienceColumns+` FROM audiences WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	if err != nil {
		return Audience{}, fmt.Errorf("database: get audience: %w", err)
	}
	return a, nil
}

func (db *DB) ListAudiences(ctx context.Context, tenantID string, limit int, after *AudienceCursor) ([]Audience, error) {
	if tenantID == "" || limit < 1 || limit > 500 {
		return nil, errors.New("database: invalid audience list arguments")
	}
	var afterTime *time.Time
	var afterID *string
	if after != nil {
		afterTime, afterID = &after.CreatedAt, &after.ID
	}
	rows, err := db.pool.Query(ctx, `SELECT `+audienceColumns+`
		FROM audiences
		WHERE tenant_id = $1
		  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3))
		ORDER BY created_at DESC, id DESC
		LIMIT $4`, tenantID, afterTime, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("database: list audiences: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []Audience
	for rows.Next() {
		a, err := scanAudience(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateAudience renames the audience. v0.35 supports no other mutable field.
func (db *DB) UpdateAudience(ctx context.Context, tenantID, id, name string) (Audience, error) {
	a, err := scanAudience(db.pool.QueryRow(ctx, `
		UPDATE audiences SET name = $3, updated_at = now()
		WHERE tenant_id = $1 AND id = $2
		RETURNING `+audienceColumns, tenantID, id, name))
	if err != nil {
		return Audience{}, fmt.Errorf("database: update audience: %w", err)
	}
	return a, nil
}

// DeleteAudience removes the audience and, via ON DELETE CASCADE, its
// membership rows ONLY — never the underlying contacts.
func (db *DB) DeleteAudience(ctx context.Context, tenantID, id string) error {
	tag, err := db.pool.Exec(ctx, `DELETE FROM audiences WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("database: delete audience: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AddAudienceMember inserts a membership row inside ONE transaction that also
// confirms both the audience and the contact belong to tenantID — the
// composite FKs make a cross-tenant row impossible even without this check,
// but this gives a clean ErrNotFound instead of a generic FK-violation error,
// and never discloses WHICH resource was missing/foreign.
func (db *DB) AddAudienceMember(ctx context.Context, tenantID, audienceID, contactID string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin add member: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM audiences WHERE tenant_id = $1 AND id = $2`, tenantID, audienceID).Scan(&one); err != nil {
		return fmt.Errorf("database: check audience: %w", normalizeErr(err))
	}
	if err := tx.QueryRow(ctx, `SELECT 1 FROM contacts WHERE tenant_id = $1 AND id = $2`, tenantID, contactID).Scan(&one); err != nil {
		return fmt.Errorf("database: check contact: %w", normalizeErr(err))
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO audience_members (audience_id, contact_id, tenant_id) VALUES ($1,$2,$3)
		ON CONFLICT (audience_id, contact_id) DO NOTHING`, audienceID, contactID, tenantID)
	if err != nil {
		return fmt.Errorf("database: add member: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return nil // already a member: idempotent success, matches suppression's create-if-absent convention
	}
	return tx.Commit(ctx)
}

// RemoveAudienceMember deletes only the membership row: it never touches the
// contact, its suppression state, or any historical data.
func (db *DB) RemoveAudienceMember(ctx context.Context, tenantID, audienceID, contactID string) error {
	tag, err := db.pool.Exec(ctx, `
		DELETE FROM audience_members
		WHERE tenant_id = $1 AND audience_id = $2 AND contact_id = $3`, tenantID, audienceID, contactID)
	if err != nil {
		return fmt.Errorf("database: remove member: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListAudienceMembers returns the audience's contacts, tenant-scoped and
// keyset-paginated by the MEMBERSHIP row's own created_at/contact_id (stable
// even if a contact's own created_at differs). Confirms the audience belongs
// to tenantID first so an unknown/foreign audience id is ErrNotFound rather
// than a silently empty page.
func (db *DB) ListAudienceMembers(ctx context.Context, tenantID, audienceID string, limit int, after *MembershipCursor) ([]Contact, error) {
	if tenantID == "" || audienceID == "" || limit < 1 || limit > 500 {
		return nil, errors.New("database: invalid audience member list arguments")
	}
	var one int
	if err := db.pool.QueryRow(ctx, `SELECT 1 FROM audiences WHERE tenant_id = $1 AND id = $2`, tenantID, audienceID).Scan(&one); err != nil {
		return nil, fmt.Errorf("database: check audience: %w", normalizeErr(err))
	}
	var afterTime *time.Time
	var afterID *string
	if after != nil {
		afterTime, afterID = &after.CreatedAt, &after.ContactID
	}
	rows, err := db.pool.Query(ctx, `SELECT `+joinedContactColumns()+`
		FROM audience_members m JOIN contacts c ON c.id = m.contact_id
		WHERE m.tenant_id = $1 AND m.audience_id = $2
		  AND ($3::timestamptz IS NULL OR (m.created_at, m.contact_id) > ($3, $4))
		ORDER BY m.created_at, m.contact_id
		LIMIT $5`, tenantID, audienceID, afterTime, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("database: list audience members: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []Contact
	for rows.Next() {
		c, err := scanContact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func joinedContactColumns() string {
	return "c.id, c.tenant_id, c.email, c.name, c.attributes, c.created_at, c.updated_at"
}
