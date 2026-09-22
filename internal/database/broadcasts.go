package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Broadcast is a durable bulk-send orchestration record (v0.36). See
// docs/design-v0.36.md for the full state machine and snapshot semantics.
type Broadcast struct {
	ID              string
	TenantID        string
	AudienceID      string
	TemplateID      string
	Name            string
	FromAddress     string
	ReplyTo         string
	SubjectTemplate string
	TextTemplate    string
	HTMLTemplate    string
	Variables       map[string]string
	Status          string // accepted | expanding | completed | failed
	FailureReason   string
	// SendAt is nil for immediate (unchanged v0.36 behavior) or the instant
	// below which expansion must never start (v0.37). It never affects
	// AudienceSnapshotAt.
	SendAt             *time.Time
	AudienceSnapshotAt time.Time
	SnapshotComplete   bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type NewBroadcast struct {
	TenantID        string
	AudienceID      string
	TemplateID      string
	Name            string
	FromAddress     string
	ReplyTo         string
	SubjectTemplate string
	TextTemplate    string
	HTMLTemplate    string
	Variables       map[string]string
	// SendAt is nil for immediate expansion (unchanged v0.36 behavior) or a
	// future instant; validated (RFC3339, not-in-past) by the API layer.
	SendAt *time.Time
	// IdempotencyCompletion, if set, completes that claim IN THE SAME
	// transaction as the broadcast insert (mirrors InsertMessage).
	IdempotencyCompletion *IdempotencyCompletion
}

type BroadcastCursor struct {
	CreatedAt time.Time
	ID        string
}

// BroadcastRecipient is one durable (broadcast, contact) snapshot +
// materialization-progress row.
type BroadcastRecipient struct {
	ID          string
	BroadcastID string
	ContactID   string
	Email       string
	Name        string
	Attributes  map[string]string
	Status      string // pending | suppressed | materialized | failed
	MessageID   *string
	Attempts    int
	CreatedAt   time.Time
}

type BroadcastRecipientCursor struct {
	CreatedAt time.Time
	ID        string
}

const broadcastColumns = `id, tenant_id, audience_id, template_id, name, from_address, reply_to,
	subject_template, text_template, html_template, variables, status, coalesce(failure_reason,''),
	send_at, audience_snapshot_at, snapshot_complete, created_at, updated_at`

func scanBroadcast(row rowScanner) (Broadcast, error) {
	var b Broadcast
	var vars []byte
	err := row.Scan(&b.ID, &b.TenantID, &b.AudienceID, &b.TemplateID, &b.Name, &b.FromAddress, &b.ReplyTo,
		&b.SubjectTemplate, &b.TextTemplate, &b.HTMLTemplate, &vars, &b.Status, &b.FailureReason,
		&b.SendAt, &b.AudienceSnapshotAt, &b.SnapshotComplete, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return Broadcast{}, normalizeErr(err)
	}
	if len(vars) > 0 {
		_ = json.Unmarshal(vars, &b.Variables)
	}
	return b, nil
}

// CreateBroadcast persists the durable acceptance record: template SOURCE
// and audience membership are NOT copied here (that is bounded, async
// expansion — see ExpandBroadcastSnapshot) — only the cheap, fixed-size
// configuration snapshot (template text, variables, from/reply-to) plus the
// audience_snapshot_at watermark that makes the later async scan
// deterministic. This is the entire durable boundary behind the 202: it is
// enough, on its own, for the expansion poller to complete this broadcast
// even across a crash, without anything else having happened yet.
// audience_snapshot_at is stamped by PostgreSQL's own now() (not a Go
// timestamp) so the later watermark comparison in SnapshotBroadcastBatch
// never suffers Go/DB clock skew — both sides of that comparison are always
// the same clock.
func (db *DB) CreateBroadcast(ctx context.Context, in NewBroadcast) (Broadcast, error) {
	id, err := newID()
	if err != nil {
		return Broadcast{}, err
	}
	vars := in.Variables
	if vars == nil {
		vars = map[string]string{}
	}
	varsJSON, err := json.Marshal(vars)
	if err != nil {
		return Broadcast{}, fmt.Errorf("database: encode broadcast variables: %w", err)
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return Broadcast{}, fmt.Errorf("database: begin create broadcast: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	b, err := scanBroadcast(tx.QueryRow(ctx, `
		INSERT INTO broadcasts (id, tenant_id, audience_id, template_id, name, from_address, reply_to,
			subject_template, text_template, html_template, variables, send_at, audience_snapshot_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12, now())
		RETURNING `+broadcastColumns,
		id, in.TenantID, in.AudienceID, in.TemplateID, in.Name, in.FromAddress, in.ReplyTo,
		in.SubjectTemplate, in.TextTemplate, in.HTMLTemplate, varsJSON, in.SendAt))
	if err != nil {
		return Broadcast{}, fmt.Errorf("database: create broadcast: %w", normalizeErr(err))
	}

	if ic := in.IdempotencyCompletion; ic != nil {
		tag, err := tx.Exec(ctx, `
			UPDATE idempotency_keys SET status = 'completed', resource_id = $1, completed_at = now()
			WHERE tenant_id = $2 AND operation = $3 AND idempotency_key = $4
			  AND status = 'in_progress' AND fingerprint = $5`,
			b.ID, in.TenantID, ic.Operation, ic.IdempotencyKey, ic.Fingerprint)
		if err != nil {
			return Broadcast{}, fmt.Errorf("database: complete idempotency key: %w", normalizeErr(err))
		}
		if tag.RowsAffected() == 0 {
			return Broadcast{}, fmt.Errorf("database: idempotency claim %s/%s no longer owned: %w", ic.Operation, ic.IdempotencyKey, ErrConflict)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Broadcast{}, fmt.Errorf("database: commit create broadcast: %w", normalizeErr(err))
	}
	return b, nil
}

func (db *DB) GetBroadcast(ctx context.Context, tenantID, id string) (Broadcast, error) {
	b, err := scanBroadcast(db.pool.QueryRow(ctx,
		`SELECT `+broadcastColumns+` FROM broadcasts WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	if err != nil {
		return Broadcast{}, fmt.Errorf("database: get broadcast: %w", err)
	}
	return b, nil
}

func (db *DB) ListBroadcasts(ctx context.Context, tenantID string, limit int, after *BroadcastCursor) ([]Broadcast, error) {
	if tenantID == "" || limit < 1 || limit > 500 {
		return nil, errors.New("database: invalid broadcast list arguments")
	}
	var afterTime *time.Time
	var afterID *string
	if after != nil {
		afterTime, afterID = &after.CreatedAt, &after.ID
	}
	rows, err := db.pool.Query(ctx, `SELECT `+broadcastColumns+`
		FROM broadcasts
		WHERE tenant_id = $1
		  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3))
		ORDER BY created_at DESC, id DESC
		LIMIT $4`, tenantID, afterTime, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("database: list broadcasts: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []Broadcast
	for rows.Next() {
		b, err := scanBroadcast(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func scanBroadcastRecipient(row rowScanner) (BroadcastRecipient, error) {
	var r BroadcastRecipient
	var attrs []byte
	err := row.Scan(&r.ID, &r.BroadcastID, &r.ContactID, &r.Email, &r.Name, &attrs, &r.Status, &r.MessageID, &r.Attempts, &r.CreatedAt)
	if err != nil {
		return BroadcastRecipient{}, normalizeErr(err)
	}
	if len(attrs) > 0 {
		_ = json.Unmarshal(attrs, &r.Attributes)
	}
	return r, nil
}

const broadcastRecipientColumns = `id, broadcast_id, contact_id, email, name, attributes, status, message_id, attempts, created_at`

// ListBroadcastRecipients is tenant-scoped via the broadcast row itself
// (confirmed first, so an unknown/foreign broadcast id is ErrNotFound, not a
// silently empty page) and keyset-paginated — never returns the whole set.
func (db *DB) ListBroadcastRecipients(ctx context.Context, tenantID, broadcastID string, limit int, after *BroadcastRecipientCursor) ([]BroadcastRecipient, error) {
	if tenantID == "" || broadcastID == "" || limit < 1 || limit > 500 {
		return nil, errors.New("database: invalid broadcast recipient list arguments")
	}
	var one int
	if err := db.pool.QueryRow(ctx, `SELECT 1 FROM broadcasts WHERE tenant_id = $1 AND id = $2`, tenantID, broadcastID).Scan(&one); err != nil {
		return nil, fmt.Errorf("database: check broadcast: %w", normalizeErr(err))
	}
	var afterTime *time.Time
	var afterID *string
	if after != nil {
		afterTime, afterID = &after.CreatedAt, &after.ID
	}
	rows, err := db.pool.Query(ctx, `SELECT `+broadcastRecipientColumns+`
		FROM broadcast_recipients
		WHERE broadcast_id = $1
		  AND ($2::timestamptz IS NULL OR (created_at, id) > ($2, $3))
		ORDER BY created_at, id
		LIMIT $4`, broadcastID, afterTime, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("database: list broadcast recipients: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []BroadcastRecipient
	for rows.Next() {
		r, err := scanBroadcastRecipient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
