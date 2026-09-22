package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Ferousco-dev/mailx/internal/routing"
)

// SendingPool is operator-managed routing policy (v0.39). Deletion is not
// supported: Enabled=false is the only lifecycle transition, so a pool's
// id can be referenced forever by historical messages/attempts without
// ever going dangling.
type SendingPool struct {
	ID        string
	Name      string
	Enabled   bool
	CreatedAt time.Time
}

// SendingPoolMemberKind distinguishes a direct-delivery identity (own
// hostname/source IP) from a relay member, which carries no per-member
// identity at all and means "use the process's single existing
// MAILX_RELAY_* config" (no per-member credentials are ever stored).
type SendingPoolMemberKind string

const (
	SendingPoolMemberDirect SendingPoolMemberKind = "direct"
	SendingPoolMemberRelay  SendingPoolMemberKind = "relay"
)

// SendingPoolMember is one concrete outbound identity within a pool. Like
// SendingPool, it supports disable-only lifecycle management: no delete.
type SendingPoolMember struct {
	ID        string
	PoolID    string
	Kind      SendingPoolMemberKind
	Hostname  *string
	SourceIP  *string
	Enabled   bool
	CreatedAt time.Time
}

func scanSendingPool(row rowScanner) (SendingPool, error) {
	var p SendingPool
	err := row.Scan(&p.ID, &p.Name, &p.Enabled, &p.CreatedAt)
	return p, normalizeErr(err)
}

func scanSendingPoolMember(row rowScanner) (SendingPoolMember, error) {
	var m SendingPoolMember
	var kind string
	err := row.Scan(&m.ID, &m.PoolID, &kind, &m.Hostname, &m.SourceIP, &m.Enabled, &m.CreatedAt)
	m.Kind = SendingPoolMemberKind(kind)
	return m, normalizeErr(err)
}

// CreateSendingPool creates an operator-managed pool. Pools are global
// infrastructure, never tenant-owned or tenant-created — callers of this
// function must be operator-only code paths (CLI), never an ordinary
// tenant-facing API handler.
func (db *DB) CreateSendingPool(ctx context.Context, name string) (SendingPool, error) {
	if name == "" {
		return SendingPool{}, errors.New("database: sending pool name is empty")
	}
	id, err := newID()
	if err != nil {
		return SendingPool{}, err
	}
	row := db.pool.QueryRow(ctx, `INSERT INTO sending_pools (id, name) VALUES ($1, $2)
		RETURNING id, name, enabled, created_at`, id, name)
	p, err := scanSendingPool(row)
	if err != nil {
		return SendingPool{}, fmt.Errorf("database: create sending pool: %w", err)
	}
	return p, nil
}

// SetSendingPoolEnabled is the pool-level kill switch. Disabling a pool
// blocks future member selection into it (see InsertMessage) and, via
// MemberRoutingEnabled, blocks new SMTP attempts for messages already
// routed through any of its members — without altering those messages'
// persisted sending_member_id.
func (db *DB) SetSendingPoolEnabled(ctx context.Context, id string, enabled bool) error {
	tag, err := db.pool.Exec(ctx, `UPDATE sending_pools SET enabled = $2 WHERE id = $1`, id, enabled)
	if err != nil {
		return fmt.Errorf("database: set sending pool enabled: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (db *DB) GetSendingPool(ctx context.Context, id string) (SendingPool, error) {
	row := db.pool.QueryRow(ctx, `SELECT id, name, enabled, created_at FROM sending_pools WHERE id = $1`, id)
	p, err := scanSendingPool(row)
	if err != nil {
		return SendingPool{}, normalizeErr(err)
	}
	return p, nil
}

func (db *DB) ListSendingPools(ctx context.Context) ([]SendingPool, error) {
	rows, err := db.pool.Query(ctx, `SELECT id, name, enabled, created_at FROM sending_pools ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("database: list sending pools: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []SendingPool
	for rows.Next() {
		p, err := scanSendingPool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, normalizeErr(rows.Err())
}

// CreateSendingPoolMember adds a member to poolID. hostname/sourceIP must
// be nil for a relay member (enforced by
// sending_pool_members_relay_no_identity_check) and are the direct-dial
// EHLO identity and net.Dialer.LocalAddr source for a direct member.
func (db *DB) CreateSendingPoolMember(ctx context.Context, poolID string, kind SendingPoolMemberKind, hostname, sourceIP *string) (SendingPoolMember, error) {
	if poolID == "" {
		return SendingPoolMember{}, errors.New("database: sending pool member requires a pool id")
	}
	if kind != SendingPoolMemberDirect && kind != SendingPoolMemberRelay {
		return SendingPoolMember{}, fmt.Errorf("database: unrecognized sending pool member kind %q", kind)
	}
	id, err := newID()
	if err != nil {
		return SendingPoolMember{}, err
	}
	row := db.pool.QueryRow(ctx, `INSERT INTO sending_pool_members (id, pool_id, kind, hostname, source_ip)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, pool_id, kind, hostname, host(source_ip), enabled, created_at`,
		id, poolID, string(kind), hostname, sourceIP)
	m, err := scanSendingPoolMember(row)
	if err != nil {
		return SendingPoolMember{}, fmt.Errorf("database: create sending pool member: %w", normalizeErr(err))
	}
	return m, nil
}

// SetSendingPoolMemberEnabled is the member-level kill switch — see
// SetSendingPoolEnabled's doc for the semantics it must preserve.
func (db *DB) SetSendingPoolMemberEnabled(ctx context.Context, id string, enabled bool) error {
	tag, err := db.pool.Exec(ctx, `UPDATE sending_pool_members SET enabled = $2 WHERE id = $1`, id, enabled)
	if err != nil {
		return fmt.Errorf("database: set sending pool member enabled: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (db *DB) GetSendingPoolMember(ctx context.Context, id string) (SendingPoolMember, error) {
	row := db.pool.QueryRow(ctx, `SELECT id, pool_id, kind, hostname, host(source_ip), enabled, created_at
		FROM sending_pool_members WHERE id = $1`, id)
	m, err := scanSendingPoolMember(row)
	if err != nil {
		return SendingPoolMember{}, normalizeErr(err)
	}
	return m, nil
}

func (db *DB) ListSendingPoolMembers(ctx context.Context, poolID string) ([]SendingPoolMember, error) {
	rows, err := db.pool.Query(ctx, `SELECT id, pool_id, kind, hostname, host(source_ip), enabled, created_at
		FROM sending_pool_members WHERE pool_id = $1 ORDER BY created_at, id`, poolID)
	if err != nil {
		return nil, fmt.Errorf("database: list sending pool members: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []SendingPoolMember
	for rows.Next() {
		m, err := scanSendingPoolMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, normalizeErr(rows.Err())
}

// AssignDomainPool sets or clears (poolID == nil) a domain's operator-
// controlled sending pool. This is infrastructure policy, not an ordinary
// tenant-writable domain attribute: callers must be operator-only code
// paths (CLI), never exposed through the tenant domain create/update API.
func (db *DB) AssignDomainPool(ctx context.Context, domainID string, poolID *string) error {
	tag, err := db.pool.Exec(ctx, `UPDATE domains SET sending_pool_id = $2 WHERE id = $1`, domainID, poolID)
	if err != nil {
		return fmt.Errorf("database: assign domain sending pool: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MemberRoutingEnabled reports whether member id may be used for a NEW SMTP
// attempt right now: both the member itself and its owning pool must be
// enabled. It never influences which member a message is routed to
// (that decision, once made, is immutable) — only whether an attempt
// through the already-decided member may proceed. Callers hold the
// message on false (existing outbox/claim release mechanism), performing
// zero SMTP and recording zero attempts, and resume automatically once
// re-enabled — never rerouting to a different member.
func (db *DB) MemberRoutingEnabled(ctx context.Context, memberID string) (bool, error) {
	var enabled bool
	err := db.pool.QueryRow(ctx, `
		SELECT m.enabled AND p.enabled
		FROM sending_pool_members m JOIN sending_pools p ON p.id = m.pool_id
		WHERE m.id = $1`, memberID).Scan(&enabled)
	if err != nil {
		return false, fmt.Errorf("database: check member routing enabled: %w", normalizeErr(err))
	}
	return enabled, nil
}

// selectRoutingMember is InsertMessage's acceptance-time route selection,
// run inside the same transaction as the message insert so the decision
// and the message become durable together. domainSendingPoolID is the
// SenderDomain's sending_pool_id, already read under the domain's FOR
// SHARE lock. A nil/empty pool, a disabled pool, or a pool with no
// currently-enabled members all resolve to "" (no member) — the message
// falls back to the legacy, pre-v0.39 default routing path exactly as if
// no pool had ever been configured. This is a graceful degrade, not a
// failure: an operator misconfiguration must never block message
// acceptance.
func selectRoutingMember(ctx context.Context, tx pgx.Tx, messageID string, domainSendingPoolID *string) (string, error) {
	if domainSendingPoolID == nil || *domainSendingPoolID == "" {
		return "", nil
	}
	var poolEnabled bool
	err := tx.QueryRow(ctx, `SELECT enabled FROM sending_pools WHERE id = $1`, *domainSendingPoolID).Scan(&poolEnabled)
	if errors.Is(normalizeErr(err), ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("database: check sending pool enabled: %w", normalizeErr(err))
	}
	if !poolEnabled {
		return "", nil
	}

	rows, err := tx.Query(ctx, `SELECT id FROM sending_pool_members WHERE pool_id = $1 AND enabled`, *domainSendingPoolID)
	if err != nil {
		return "", fmt.Errorf("database: list enabled sending pool members: %w", normalizeErr(err))
	}
	defer rows.Close()
	var candidates []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", fmt.Errorf("database: scan sending pool member id: %w", err)
		}
		candidates = append(candidates, id)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("database: list enabled sending pool members: %w", normalizeErr(err))
	}

	return routing.SelectMember(messageID, candidates), nil
}
