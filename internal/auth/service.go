package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
)

// Distinct internally so logs/tests can tell WHY authentication failed;
// internal/api's middleware collapses every one of these (except
// ErrUnavailable) into the same generic 401 response — see
// internal/api/authmiddleware.go's doc for why they must not be
// distinguishable externally.
var (
	ErrUnknownKey  = errors.New("auth: unknown key")
	ErrWrongSecret = errors.New("auth: wrong secret")
	ErrRevoked     = errors.New("auth: key revoked")
	ErrExpired     = errors.New("auth: key expired")
	// ErrUnavailable means authentication could not consult its source of
	// truth — callers must fail closed (503), never treat this as "valid".
	ErrUnavailable = errors.New("auth: authentication temporarily unavailable")
)

// touchThreshold bounds last_used_at write amplification: a key hit
// thousands of times a minute still produces at most one UPDATE every 5
// minutes. Precision is deliberately approximate — see the package doc's
// "last used" note — correctness of authentication itself never depends
// on this value being exact or even present.
const touchThreshold = 5 * time.Minute

type apiKeyStore interface {
	InsertAPIKey(ctx context.Context, in database.NewAPIKey) (database.APIKey, error)
	GetAPIKeyByKeyID(ctx context.Context, keyID string) (database.APIKey, error)
	ListAPIKeysForTenant(ctx context.Context, tenantID string) ([]database.APIKey, error)
	TouchAPIKeyLastUsed(ctx context.Context, id string, at time.Time) error
	RevokeAPIKey(ctx context.Context, id string) error
	RotateAPIKey(ctx context.Context, oldID string, newKey database.NewAPIKey, now, retireAt time.Time) (database.APIKey, error)
}

// Service is the only place that ties key generation/verification
// (apikey.go) to durable storage (internal/database). internal/api's
// authentication middleware depends on this, never on internal/database
// directly, keeping "how a credential is verified" in one place.
type Service struct {
	db     apiKeyStore
	pepper []byte
	now    func() time.Time
}

// NewService constructs a Service. pepper may be nil/empty — see
// apikey.go's package doc for exactly what that trades away; it is a
// deliberate, documented choice for local development, not an oversight.
func NewService(db apiKeyStore, pepper []byte) *Service {
	return &Service{db: db, pepper: pepper, now: func() time.Time { return time.Now().UTC() }}
}

// Create generates a new credential for tenantID and persists it. The
// returned Generated.Raw is the ONLY time the raw key ever exists outside
// the caller's own memory — see apikey.go's Generate doc.
func (s *Service) Create(ctx context.Context, tenantID, name string, scopes []string, ttl *time.Duration) (Generated, database.APIKey, error) {
	if err := validateScopes(scopes); err != nil {
		return Generated{}, database.APIKey{}, err
	}
	gen, err := Generate(s.pepper)
	if err != nil {
		return Generated{}, database.APIKey{}, err
	}
	var expiresAt *time.Time
	if ttl != nil {
		t := s.now().Add(*ttl)
		expiresAt = &t
	}
	record, err := s.db.InsertAPIKey(ctx, database.NewAPIKey{
		TenantID: tenantID, Name: name, KeyID: gen.KeyID, SecretHash: gen.SecretHash,
		Scopes: scopes, ExpiresAt: expiresAt,
	})
	if err != nil {
		// Nothing durable references gen.Raw — it is simply discarded; the
		// caller sees the DB error and can retry create from scratch. There
		// is no "credential MailX can't authenticate" state to worry about
		// because gen.Raw and the DB row are created together, right here.
		return Generated{}, database.APIKey{}, fmt.Errorf("auth: create api key: %w", err)
	}
	return gen, record, nil
}

// Authenticate parses raw, locates its key row, and verifies the secret.
// A PostgreSQL failure returns ErrUnavailable, never a false success —
// see the package doc's "database failure" guidance: authentication must
// fail closed, not treat an unreachable source of truth as permission.
func (s *Service) Authenticate(ctx context.Context, raw string) (database.APIKey, error) {
	keyID, secret, err := Parse(raw)
	if err != nil {
		return database.APIKey{}, ErrUnknownKey
	}
	record, err := s.db.GetAPIKeyByKeyID(ctx, keyID)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return database.APIKey{}, ErrUnknownKey
		}
		return database.APIKey{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if !Verify(secret, record.SecretHash, s.pepper) {
		return database.APIKey{}, ErrWrongSecret
	}
	now := s.now()
	if record.RevokedAt != nil {
		return database.APIKey{}, ErrRevoked
	}
	if record.ExpiresAt != nil && !record.ExpiresAt.After(now) {
		return database.APIKey{}, ErrExpired
	}

	if record.LastUsedAt == nil || now.Sub(*record.LastUsedAt) > touchThreshold {
		// Best-effort: a failed touch never fails authentication itself —
		// last_used_at is observability, not a correctness input. Update
		// the in-memory copy too so the caller sees the value that was
		// (or was just attempted to be) persisted, not a stale nil/older
		// timestamp from before this call.
		if err := s.db.TouchAPIKeyLastUsed(ctx, record.ID, now); err == nil {
			record.LastUsedAt = &now
		}
	}
	return record, nil
}

// Revoke durably invalidates keyID immediately. keyID is the credential's
// public, non-secret identifier (the middle segment of "mx_<key_id>_..."),
// which is what a developer/CLI operator actually has - not the internal
// database row id.
func (s *Service) Revoke(ctx context.Context, keyID string) error {
	record, err := s.db.GetAPIKeyByKeyID(ctx, keyID)
	if err != nil {
		return err
	}
	return s.db.RevokeAPIKey(ctx, record.ID)
}

// Rotate creates a brand-new credential for the same tenant, inheriting
// the old key's name/scopes, and retires the old one after grace (zero
// for immediate invalidation) — see database.DB.RotateAPIKey's doc for
// the crash-safety guarantee (one transaction, both effects or neither).
func (s *Service) Rotate(ctx context.Context, keyID string, grace time.Duration) (Generated, database.APIKey, error) {
	old, err := s.db.GetAPIKeyByKeyID(ctx, keyID)
	if err != nil {
		return Generated{}, database.APIKey{}, err
	}
	now := s.now()
	// Rotating an already-dead key is meaningless and dangerous to do
	// silently: copying its expiry to the replacement would hand back an
	// immediately-expired credential, and moving ITS OWN expiry forward
	// by `grace` would revive an already-expired (or revoked) secret for
	// the grace window - exactly the opposite of what grace is for.
	// Create a fresh key instead.
	if old.RevokedAt != nil {
		// Wrapped in ErrRevoked (CodeRabbit, PR #28) so a caller like the
		// HTTP rotate handler can distinguish "key is dead" from a generic
		// failure and answer 409, not 500, for the race where a key is
		// revoked/expired between an unlocked pre-check and this call.
		return Generated{}, database.APIKey{}, fmt.Errorf("auth: cannot rotate a revoked key; create a new one instead: %w", ErrRevoked)
	}
	if old.ExpiresAt != nil && !old.ExpiresAt.After(now) {
		return Generated{}, database.APIKey{}, fmt.Errorf("auth: cannot rotate an already-expired key; create a new one instead: %w", ErrExpired)
	}
	gen, err := Generate(s.pepper)
	if err != nil {
		return Generated{}, database.APIKey{}, err
	}
	created, err := s.db.RotateAPIKey(ctx, old.ID, database.NewAPIKey{
		TenantID: old.TenantID, Name: old.Name, KeyID: gen.KeyID, SecretHash: gen.SecretHash,
		Scopes: old.Scopes, ExpiresAt: old.ExpiresAt,
	}, now, now.Add(grace))
	if err != nil {
		// The old key is untouched (transaction rolled back) — it remains
		// valid, and gen.Raw was never persisted, so it simply cannot
		// authenticate; nothing ambiguous survives a failed rotation.
		return Generated{}, database.APIKey{}, fmt.Errorf("auth: rotate api key: %w", err)
	}
	return gen, created, nil
}

// List returns every key (active and historical) for a tenant — metadata
// only; callers must never format APIKey.SecretHash into a response as
// if it were usable, and it never was the raw secret in the first place.
func (s *Service) List(ctx context.Context, tenantID string) ([]database.APIKey, error) {
	return s.db.ListAPIKeysForTenant(ctx, tenantID)
}

func validateScopes(scopes []string) error {
	if len(scopes) == 0 {
		return errors.New("auth: at least one scope is required")
	}
	seen := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		if !ValidScope(s) {
			return fmt.Errorf("auth: unknown scope %q", s)
		}
		if seen[s] {
			return fmt.Errorf("auth: duplicate scope %q", s)
		}
		seen[s] = true
	}
	return nil
}
