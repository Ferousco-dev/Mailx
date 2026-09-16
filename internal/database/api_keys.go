package database

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// APIKey is one durable credential row. It never carries the raw secret —
// only what authentication needs (key_id, secret_hash) and what
// management/audit needs (everything else).
type APIKey struct {
	ID           string
	TenantID     string
	Name         string
	KeyID        string
	SecretHash   string
	Scopes       []string
	CreatedAt    time.Time
	LastUsedAt   *time.Time
	ExpiresAt    *time.Time
	RevokedAt    *time.Time
	ReplacedByID *string
}

// NewAPIKey is InsertAPIKey's input. Generation (crypto/rand) and hashing
// happen in internal/auth; this package only persists the result.
type NewAPIKey struct {
	TenantID   string
	Name       string
	KeyID      string
	SecretHash string
	Scopes     []string
	ExpiresAt  *time.Time
}

func (n NewAPIKey) validate() error {
	if n.TenantID == "" {
		return errors.New("database: tenant ID is empty")
	}
	if n.Name == "" {
		return errors.New("database: api key name is empty")
	}
	if n.KeyID == "" || n.SecretHash == "" {
		return errors.New("database: api key id/secret hash is empty")
	}
	if len(n.Scopes) == 0 {
		return errors.New("database: api key has no scopes")
	}
	return nil
}

// InsertAPIKey persists one new key row. Called both for plain creation
// and as the "new row" half of rotation (see RotateAPIKey).
func (db *DB) InsertAPIKey(ctx context.Context, in NewAPIKey) (APIKey, error) {
	if err := in.validate(); err != nil {
		return APIKey{}, err
	}
	id, err := newID()
	if err != nil {
		return APIKey{}, err
	}
	var key APIKey
	err = db.pool.QueryRow(ctx, `
		INSERT INTO api_keys (id, tenant_id, name, key_id, secret_hash, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, tenant_id, name, key_id, secret_hash, scopes, created_at, last_used_at, expires_at, revoked_at, replaced_by_id`,
		id, in.TenantID, in.Name, in.KeyID, in.SecretHash, in.Scopes, in.ExpiresAt,
	).Scan(&key.ID, &key.TenantID, &key.Name, &key.KeyID, &key.SecretHash, &key.Scopes,
		&key.CreatedAt, &key.LastUsedAt, &key.ExpiresAt, &key.RevokedAt, &key.ReplacedByID)
	if err != nil {
		return APIKey{}, fmt.Errorf("database: insert api key: %w", normalizeErr(err))
	}
	return key, nil
}

// GetAPIKeyByKeyID is authentication's only query: locate the single
// candidate row for an incoming credential's non-secret identifier.
// Finding a row here is NOT authentication - the caller still must verify
// the secret against SecretHash.
func (db *DB) GetAPIKeyByKeyID(ctx context.Context, keyID string) (APIKey, error) {
	var key APIKey
	err := db.pool.QueryRow(ctx, `
		SELECT id, tenant_id, name, key_id, secret_hash, scopes, created_at, last_used_at, expires_at, revoked_at, replaced_by_id
		FROM api_keys WHERE key_id = $1`,
		keyID,
	).Scan(&key.ID, &key.TenantID, &key.Name, &key.KeyID, &key.SecretHash, &key.Scopes,
		&key.CreatedAt, &key.LastUsedAt, &key.ExpiresAt, &key.RevokedAt, &key.ReplacedByID)
	if err != nil {
		return APIKey{}, normalizeErr(err)
	}
	return key, nil
}

// ListAPIKeysForTenant returns every key (active and historical) for a
// tenant, newest first - a management/CLI view, not a hot request path.
func (db *DB) ListAPIKeysForTenant(ctx context.Context, tenantID string) ([]APIKey, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, tenant_id, name, key_id, secret_hash, scopes, created_at, last_used_at, expires_at, revoked_at, replaced_by_id
		FROM api_keys WHERE tenant_id = $1 ORDER BY created_at DESC`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("database: list api keys: %w", normalizeErr(err))
	}
	defer rows.Close()

	var out []APIKey
	for rows.Next() {
		var key APIKey
		if err := rows.Scan(&key.ID, &key.TenantID, &key.Name, &key.KeyID, &key.SecretHash, &key.Scopes,
			&key.CreatedAt, &key.LastUsedAt, &key.ExpiresAt, &key.RevokedAt, &key.ReplacedByID); err != nil {
			return nil, fmt.Errorf("database: scan api key: %w", err)
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

// TouchAPIKeyLastUsed is a throttled, best-effort write: the caller
// decides staleness (see internal/auth's precision doc) so this package
// stays a plain repository with no policy of its own.
func (db *DB) TouchAPIKeyLastUsed(ctx context.Context, id string, at time.Time) error {
	_, err := db.pool.Exec(ctx, `UPDATE api_keys SET last_used_at = $1 WHERE id = $2`, at, id)
	if err != nil {
		return fmt.Errorf("database: touch api key last used: %w", normalizeErr(err))
	}
	return nil
}

// RevokeAPIKey durably invalidates a key immediately, independent of
// expires_at. The row is kept, not deleted.
func (db *DB) RevokeAPIKey(ctx context.Context, id string) error {
	tag, err := db.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("database: revoke api key: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RotateAPIKey creates a new key row and retires the old one in ONE
// transaction: either both happen or neither does, so a crash never
// leaves a new row with no corresponding old-key retirement (or vice
// versa). retireAt is the OLD key's new expires_at, computed by the
// caller (internal/auth.Service, using its own clock) rather than this
// package calling Postgres's now() — the two must agree with whatever
// clock Authenticate later compares expires_at against, so the caller
// owns that decision entirely; retireAt == the caller's "now" makes the
// old key expire immediately. The old key's existing revoked_at (if
// already revoked) is left untouched - rotating an already-revoked key
// is allowed but does not un-revoke it.
func (db *DB) RotateAPIKey(ctx context.Context, oldID string, newKey NewAPIKey, retireAt time.Time) (APIKey, error) {
	if err := newKey.validate(); err != nil {
		return APIKey{}, err
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return APIKey{}, fmt.Errorf("database: begin rotate api key: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// SELECT ... FOR UPDATE locks the old row for the rest of this
	// transaction: a concurrent rotation of the SAME key blocks here
	// until this transaction commits or rolls back, then sees the
	// now-set replaced_by_id itself and stops — so only one of two
	// concurrent rotations can ever succeed, and the loser never inserts
	// a replacement row that would otherwise be left valid.
	var oldReplacedByID *string
	var oldRevokedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT replaced_by_id, revoked_at FROM api_keys WHERE id = $1 FOR UPDATE`, oldID).
		Scan(&oldReplacedByID, &oldRevokedAt)
	if err != nil {
		if isNoRows(err) {
			return APIKey{}, fmt.Errorf("database: rotate api key: %w", ErrNotFound)
		}
		return APIKey{}, fmt.Errorf("database: lock old api key: %w", normalizeErr(err))
	}
	if oldReplacedByID != nil || oldRevokedAt != nil {
		return APIKey{}, fmt.Errorf("database: rotate api key: key already revoked or already rotated: %w", ErrConflict)
	}

	rowID, err := newID()
	if err != nil {
		return APIKey{}, err
	}
	var created APIKey
	err = tx.QueryRow(ctx, `
		INSERT INTO api_keys (id, tenant_id, name, key_id, secret_hash, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, tenant_id, name, key_id, secret_hash, scopes, created_at, last_used_at, expires_at, revoked_at, replaced_by_id`,
		rowID, newKey.TenantID, newKey.Name, newKey.KeyID, newKey.SecretHash, newKey.Scopes, newKey.ExpiresAt,
	).Scan(&created.ID, &created.TenantID, &created.Name, &created.KeyID, &created.SecretHash, &created.Scopes,
		&created.CreatedAt, &created.LastUsedAt, &created.ExpiresAt, &created.RevokedAt, &created.ReplacedByID)
	if err != nil {
		return APIKey{}, fmt.Errorf("database: insert rotated api key: %w", normalizeErr(err))
	}

	// Guaranteed to affect exactly 1 row: we still hold the lock acquired
	// above, and nothing else could have changed this row since.
	if _, err := tx.Exec(ctx,
		`UPDATE api_keys SET expires_at = $1, replaced_by_id = $2 WHERE id = $3`,
		retireAt, created.ID, oldID,
	); err != nil {
		return APIKey{}, fmt.Errorf("database: retire old api key: %w", normalizeErr(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return APIKey{}, fmt.Errorf("database: commit rotate api key: %w", normalizeErr(err))
	}
	return created, nil
}
