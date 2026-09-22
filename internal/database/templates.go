package database

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Template is a tenant-owned reusable email template (v0.33). Durable
// configuration/content, not delivery telemetry: not retention-bound, lives
// until the tenant deletes it.
type Template struct {
	ID        string
	TenantID  string
	Name      string
	Subject   string
	Text      string
	HTML      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type NewTemplate struct {
	TenantID string
	Name     string
	Subject  string
	Text     string
	HTML     string
}

// TemplateUpdate: nil fields leave that column unchanged (PATCH semantics).
type TemplateUpdate struct {
	Name    *string
	Subject *string
	Text    *string
	HTML    *string
}

type TemplateCursor struct {
	CreatedAt time.Time
	ID        string
}

const templateColumns = `id, tenant_id, name, subject, text_body, html_body, created_at, updated_at`

func scanTemplate(row rowScanner) (Template, error) {
	var t Template
	err := row.Scan(&t.ID, &t.TenantID, &t.Name, &t.Subject, &t.Text, &t.HTML, &t.CreatedAt, &t.UpdatedAt)
	return t, normalizeErr(err)
}

// CreateTemplate inserts a new template. ErrConflict (via the unique
// (tenant_id, name) constraint) means the tenant already has a template with
// that name.
func (db *DB) CreateTemplate(ctx context.Context, in NewTemplate) (Template, error) {
	id, err := newID()
	if err != nil {
		return Template{}, err
	}
	t, err := scanTemplate(db.pool.QueryRow(ctx, `
		INSERT INTO templates (id, tenant_id, name, subject, text_body, html_body)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING `+templateColumns,
		id, in.TenantID, in.Name, in.Subject, in.Text, in.HTML))
	if err != nil {
		return Template{}, fmt.Errorf("database: create template: %w", err)
	}
	return t, nil
}

// GetTemplate is tenant-scoped in the query itself, so another tenant's
// template is indistinguishable from a missing one (ErrNotFound).
func (db *DB) GetTemplate(ctx context.Context, tenantID, id string) (Template, error) {
	t, err := scanTemplate(db.pool.QueryRow(ctx,
		`SELECT `+templateColumns+` FROM templates WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	if err != nil {
		return Template{}, fmt.Errorf("database: get template: %w", err)
	}
	return t, nil
}

func (db *DB) ListTemplates(ctx context.Context, tenantID string, limit int, after *TemplateCursor) ([]Template, error) {
	if tenantID == "" || limit < 1 || limit > 500 {
		return nil, errors.New("database: invalid template list arguments")
	}
	var afterTime *time.Time
	var afterID *string
	if after != nil {
		afterTime, afterID = &after.CreatedAt, &after.ID
	}
	rows, err := db.pool.Query(ctx, `SELECT `+templateColumns+`
		FROM templates
		WHERE tenant_id = $1
		  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3))
		ORDER BY created_at DESC, id DESC
		LIMIT $4`, tenantID, afterTime, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("database: list templates: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTemplate applies a partial update and bumps updated_at. Already
// accepted messages built from this template's earlier content are rows in
// `messages`/FileStore, entirely independent of this row — nothing here can
// reach back and change them.
func (db *DB) UpdateTemplate(ctx context.Context, tenantID, id string, u TemplateUpdate) (Template, error) {
	t, err := scanTemplate(db.pool.QueryRow(ctx, `
		UPDATE templates SET
			name = COALESCE($3, name),
			subject = COALESCE($4, subject),
			text_body = COALESCE($5, text_body),
			html_body = COALESCE($6, html_body),
			updated_at = now()
		WHERE tenant_id = $1 AND id = $2
		RETURNING `+templateColumns,
		tenantID, id, u.Name, u.Subject, u.Text, u.HTML))
	if err != nil {
		return Template{}, fmt.Errorf("database: update template: %w", err)
	}
	return t, nil
}

// DeleteTemplate is a hard delete: no versioning/soft-delete in v0.33 (see
// docs/design-v0.33.md). Messages already sent from this template keep their
// own durable content and are unaffected.
func (db *DB) DeleteTemplate(ctx context.Context, tenantID, id string) error {
	tag, err := db.pool.Exec(ctx, `DELETE FROM templates WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("database: delete template: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
