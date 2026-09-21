package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrDomainNotVerified means an operation needs a verified, active domain.
var ErrDomainNotVerified = errors.New("database: domain is not verified")

// ErrDKIMKeyPending means the domain already has a key awaiting publication.
var ErrDKIMKeyPending = errors.New("database: a DKIM key is already pending for this domain")

type DKIMStatus string

const (
	DKIMPending DKIMStatus = "pending"
	DKIMActive  DKIMStatus = "active"
	DKIMRetired DKIMStatus = "retired"
)

// DKIMKey is one signing key. PrivateCiphertext and PrivateNonce are only
// populated by ActiveSigningKey; every other query omits them.
type DKIMKey struct {
	ID, TenantID, DomainID, Selector, Algorithm string
	KeyBits                                     int
	PublicKey                                   string
	Status                                      DKIMStatus
	CreatedAt                                   time.Time
	ActivatedAt, RetiredAt                      *time.Time
	PrivateCiphertext, PrivateNonce             []byte
}

type NewDKIMKey struct {
	Selector, Algorithm, PublicKey  string
	KeyBits                         int
	PrivateCiphertext, PrivateNonce []byte
}

const dkimPublicColumns = `id, tenant_id, domain_id, selector, algorithm, key_bits, public_key, status, created_at, activated_at, retired_at`

func scanDKIM(row rowScanner) (DKIMKey, error) {
	var k DKIMKey
	err := row.Scan(&k.ID, &k.TenantID, &k.DomainID, &k.Selector, &k.Algorithm, &k.KeyBits, &k.PublicKey,
		&k.Status, &k.CreatedAt, &k.ActivatedAt, &k.RetiredAt)
	return k, normalizeErr(err)
}

// lockVerifiedDomain serializes lifecycle operations on one domain: it locks
// the row and requires it to be tenant-owned, verified and not deleted.
func lockVerifiedDomain(ctx context.Context, tx pgx.Tx, tenantID, domainID string) error {
	var status DomainStatus
	err := tx.QueryRow(ctx, `SELECT verification_status FROM domains
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL FOR UPDATE`, tenantID, domainID).Scan(&status)
	if err != nil {
		return normalizeErr(err)
	}
	if status != DomainVerified {
		return ErrDomainNotVerified
	}
	return nil
}

// CreateDKIMKey stores a new PENDING key (already encrypted by the caller). The
// partial unique index allows only one pending key per domain, so concurrent
// creations resolve to one winner and ErrDKIMKeyPending for the rest.
func (db *DB) CreateDKIMKey(ctx context.Context, tenantID, domainID string, in NewDKIMKey) (DKIMKey, error) {
	id, err := newID()
	if err != nil {
		return DKIMKey{}, err
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return DKIMKey{}, fmt.Errorf("database: begin create DKIM key: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockVerifiedDomain(ctx, tx, tenantID, domainID); err != nil {
		return DKIMKey{}, err
	}
	var key DKIMKey
	err = tx.QueryRow(ctx, `INSERT INTO dkim_keys
		(id, tenant_id, domain_id, selector, algorithm, key_bits, public_key, private_ciphertext, private_nonce, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending') RETURNING `+dkimPublicColumns,
		id, tenantID, domainID, in.Selector, in.Algorithm, in.KeyBits, in.PublicKey, in.PrivateCiphertext, in.PrivateNonce).Scan(
		&key.ID, &key.TenantID, &key.DomainID, &key.Selector, &key.Algorithm, &key.KeyBits, &key.PublicKey,
		&key.Status, &key.CreatedAt, &key.ActivatedAt, &key.RetiredAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if pgErr.ConstraintName == "uq_dkim_one_pending" {
				return DKIMKey{}, ErrDKIMKeyPending
			}
			return DKIMKey{}, ErrConflict // selector collision: caller retries with a new selector
		}
		return DKIMKey{}, fmt.Errorf("database: create DKIM key: %w", normalizeErr(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return DKIMKey{}, fmt.Errorf("database: commit DKIM key: %w", normalizeErr(err))
	}
	return key, nil
}

// ListDKIMKeys returns a domain's keys, newest first, without private material.
// It is scoped by tenant, so another tenant's domain ID yields no rows.
func (db *DB) ListDKIMKeys(ctx context.Context, tenantID, domainID string) ([]DKIMKey, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+dkimPublicColumns+` FROM dkim_keys
		WHERE tenant_id = $1 AND domain_id = $2 ORDER BY created_at DESC, id DESC`, tenantID, domainID)
	if err != nil {
		return nil, fmt.Errorf("database: list DKIM keys: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []DKIMKey
	for rows.Next() {
		k, err := scanDKIM(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ActivateDKIMKey promotes the domain's pending key to active and retires (and
// destroys the private material of) the previous active key, atomically under
// the domain lock. A concurrent activation fails on uq_dkim_one_active.
func (db *DB) ActivateDKIMKey(ctx context.Context, tenantID, domainID string, now time.Time) (DKIMKey, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return DKIMKey{}, fmt.Errorf("database: begin activate DKIM key: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockVerifiedDomain(ctx, tx, tenantID, domainID); err != nil {
		return DKIMKey{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE dkim_keys
		SET status = 'retired', retired_at = $3, private_ciphertext = NULL, private_nonce = NULL
		WHERE tenant_id = $1 AND domain_id = $2 AND status = 'active'`, tenantID, domainID, now); err != nil {
		return DKIMKey{}, fmt.Errorf("database: retire DKIM key: %w", normalizeErr(err))
	}
	row := tx.QueryRow(ctx, `UPDATE dkim_keys SET status = 'active', activated_at = $3
		WHERE tenant_id = $1 AND domain_id = $2 AND status = 'pending' RETURNING `+dkimPublicColumns, tenantID, domainID, now)
	key, err := scanDKIM(row)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return DKIMKey{}, ErrNotFound
		}
		return DKIMKey{}, fmt.Errorf("database: activate DKIM key: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return DKIMKey{}, fmt.Errorf("database: commit DKIM activation: %w", normalizeErr(err))
	}
	return key, nil
}

// ActiveSigningKey returns the tenant's ACTIVE key for a domain NAME, including
// the encrypted private key. It joins domains so a key is only reachable while
// the domain is tenant-owned, verified and not deleted. ErrNotFound means the
// domain has no active key (or no authorization).
func (db *DB) ActiveSigningKey(ctx context.Context, tenantID, domainName string) (DKIMKey, error) {
	var k DKIMKey
	err := db.pool.QueryRow(ctx, `SELECT k.id, k.tenant_id, k.domain_id, k.selector, k.algorithm, k.key_bits, k.public_key,
			k.status, k.created_at, k.activated_at, k.retired_at, k.private_ciphertext, k.private_nonce
		FROM domains d JOIN dkim_keys k ON k.domain_id = d.id AND k.tenant_id = d.tenant_id
		WHERE d.tenant_id = $1 AND d.name = $2 AND d.deleted_at IS NULL
		  AND d.verification_status = 'verified' AND k.status = 'active'`, tenantID, domainName).Scan(
		&k.ID, &k.TenantID, &k.DomainID, &k.Selector, &k.Algorithm, &k.KeyBits, &k.PublicKey,
		&k.Status, &k.CreatedAt, &k.ActivatedAt, &k.RetiredAt, &k.PrivateCiphertext, &k.PrivateNonce)
	return k, normalizeErr(err)
}

// VerifiedSenderDomain returns the tenant's verified, non-deleted domain with
// exactly this canonical name. It is the single authorization query behind
// verified-From enforcement: no suffix or parent-domain matching happens here.
func (db *DB) VerifiedSenderDomain(ctx context.Context, tenantID, name string) (Domain, error) {
	return scanDomain(db.pool.QueryRow(ctx, `SELECT `+domainColumns+` FROM domains
		WHERE tenant_id = $1 AND name = $2 AND deleted_at IS NULL AND verification_status = 'verified'`, tenantID, name))
}
