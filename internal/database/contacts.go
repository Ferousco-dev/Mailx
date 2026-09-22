package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Ferousco-dev/mailx/internal/contact"
)

// Contact is "this tenant knows this recipient" — independent of
// suppression (safety policy) and message recipients (SMTP state).
// Deleting/recreating/updating a Contact never touches either.
type Contact struct {
	ID         string
	TenantID   string
	Email      string // as submitted
	Name       string
	Attributes map[string]string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type NewContact struct {
	TenantID   string
	Email      string
	Name       string
	Attributes map[string]string
}

// ContactUpdate: nil fields leave that column unchanged.
type ContactUpdate struct {
	Email      *string
	Name       *string
	Attributes map[string]string // nil = unchanged; non-nil (incl. empty) replaces
}

type ContactCursor struct {
	CreatedAt time.Time
	ID        string
}

var ErrInvalidContact = errors.New("database: invalid contact")

const contactColumns = `id, tenant_id, email, name, attributes, created_at, updated_at`

func scanContact(row rowScanner) (Contact, error) {
	var c Contact
	var attrs []byte
	err := row.Scan(&c.ID, &c.TenantID, &c.Email, &c.Name, &attrs, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return Contact{}, normalizeErr(err)
	}
	if len(attrs) > 0 {
		if err := json.Unmarshal(attrs, &c.Attributes); err != nil {
			return Contact{}, fmt.Errorf("database: decode contact attributes: %w", err)
		}
	}
	return c, nil
}

// CreateContact inserts a new contact. ErrConflict (via the unique
// (tenant_id, normalized_email) constraint) means the tenant already has a
// contact with that identity — v0.34 has no upsert.
func (db *DB) CreateContact(ctx context.Context, in NewContact) (Contact, error) {
	key, err := contact.Normalize(in.Email)
	if err != nil {
		return Contact{}, ErrInvalidContact
	}
	m := in.Attributes
	if m == nil {
		m = map[string]string{}
	}
	attrs, err := json.Marshal(m)
	if err != nil {
		return Contact{}, ErrInvalidContact
	}
	id, err := newID()
	if err != nil {
		return Contact{}, err
	}
	c, err := scanContact(db.pool.QueryRow(ctx, `
		INSERT INTO contacts (id, tenant_id, normalized_email, email, name, attributes)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING `+contactColumns,
		id, in.TenantID, key, in.Email, in.Name, attrs))
	if err != nil {
		return Contact{}, fmt.Errorf("database: create contact: %w", err)
	}
	return c, nil
}

func (db *DB) GetContact(ctx context.Context, tenantID, id string) (Contact, error) {
	c, err := scanContact(db.pool.QueryRow(ctx,
		`SELECT `+contactColumns+` FROM contacts WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	if err != nil {
		return Contact{}, fmt.Errorf("database: get contact: %w", err)
	}
	return c, nil
}

func (db *DB) ListContacts(ctx context.Context, tenantID string, limit int, after *ContactCursor) ([]Contact, error) {
	if tenantID == "" || limit < 1 || limit > 500 {
		return nil, errors.New("database: invalid contact list arguments")
	}
	var afterTime *time.Time
	var afterID *string
	if after != nil {
		afterTime, afterID = &after.CreatedAt, &after.ID
	}
	rows, err := db.pool.Query(ctx, `SELECT `+contactColumns+`
		FROM contacts
		WHERE tenant_id = $1
		  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3))
		ORDER BY created_at DESC, id DESC
		LIMIT $4`, tenantID, afterTime, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("database: list contacts: %w", normalizeErr(err))
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

// UpdateContact applies a partial update. Changing Email re-validates and
// re-checks uniqueness under the NEW identity; it never touches suppressions
// (they key on their own normalization, entirely independent).
func (db *DB) UpdateContact(ctx context.Context, tenantID, id string, u ContactUpdate) (Contact, error) {
	var email, key *string
	if u.Email != nil {
		k, err := contact.Normalize(*u.Email)
		if err != nil {
			return Contact{}, ErrInvalidContact
		}
		email, key = u.Email, &k
	}
	var attrs []byte
	if u.Attributes != nil {
		b, err := json.Marshal(u.Attributes)
		if err != nil {
			return Contact{}, ErrInvalidContact
		}
		attrs = b
	}
	c, err := scanContact(db.pool.QueryRow(ctx, `
		UPDATE contacts SET
			email = COALESCE($3, email),
			normalized_email = COALESCE($4, normalized_email),
			name = COALESCE($5, name),
			attributes = CASE WHEN $6::jsonb IS NULL THEN attributes ELSE $6::jsonb END,
			updated_at = now()
		WHERE tenant_id = $1 AND id = $2
		RETURNING `+contactColumns,
		tenantID, id, email, key, u.Name, nullableJSON(attrs)))
	if err != nil {
		return Contact{}, fmt.Errorf("database: update contact: %w", err)
	}
	return c, nil
}

func nullableJSON(b []byte) any {
	if b == nil {
		return nil
	}
	return b
}

// DeleteContact is a hard delete. It touches ONLY the contacts row: no
// suppression, no message/recipient/delivery history, no feedback, no
// webhook row references a contact, so nothing else can cascade.
func (db *DB) DeleteContact(ctx context.Context, tenantID, id string) error {
	tag, err := db.pool.Exec(ctx, `DELETE FROM contacts WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("database: delete contact: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
