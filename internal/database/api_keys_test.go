package database

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func sampleNewAPIKey(tenantID string) NewAPIKey {
	return NewAPIKey{
		TenantID: tenantID, Name: "test key", KeyID: "keyid00000000000000000000000001",
		SecretHash: "hash0000000000000000000000000000000000000000000000000000000001",
		Scopes:     []string{"emails:send"},
	}
}

func TestInsertAPIKeyRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	in := sampleNewAPIKey(tenant.ID)
	key, err := db.InsertAPIKey(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if key.ID == "" || key.RevokedAt != nil || key.LastUsedAt != nil {
		t.Fatalf("unexpected new key state: %+v", key)
	}

	got, err := db.GetAPIKeyByKeyID(ctx, in.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SecretHash != in.SecretHash || len(got.Scopes) != 1 || got.Scopes[0] != "emails:send" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestInsertAPIKeyRejectsUnknownScope(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	in := sampleNewAPIKey(tenant.ID)
	in.Scopes = []string{"bogus:scope"}
	if _, err := db.InsertAPIKey(ctx, in); err == nil {
		t.Fatal("expected an unknown scope to be rejected at the database level")
	}
}

func TestInsertAPIKeyDuplicateKeyIDConflicts(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	in := sampleNewAPIKey(tenant.ID)
	if _, err := db.InsertAPIKey(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertAPIKey(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict on duplicate key_id, got %v", err)
	}
}

func TestGetAPIKeyByKeyIDUnknownIsNotFound(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.GetAPIKeyByKeyID(context.Background(), "does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestListAPIKeysForTenantIsolatesAndOrdersNewestFirst(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenantA := newTestTenant(t, db)
	tenantB, err := db.CreateTenant(ctx, "tenant-b")
	if err != nil {
		t.Fatal(err)
	}

	first := sampleNewAPIKey(tenantA.ID)
	first.KeyID = "keyid00000000000000000000000002"
	if _, err := db.InsertAPIKey(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := sampleNewAPIKey(tenantA.ID)
	second.KeyID = "keyid00000000000000000000000003"
	if _, err := db.InsertAPIKey(ctx, second); err != nil {
		t.Fatal(err)
	}
	other := sampleNewAPIKey(tenantB.ID)
	other.KeyID = "keyid00000000000000000000000004"
	if _, err := db.InsertAPIKey(ctx, other); err != nil {
		t.Fatal(err)
	}

	keys, err := db.ListAPIKeysForTenant(ctx, tenantA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys for tenant A, got %d", len(keys))
	}
	if keys[0].KeyID != second.KeyID {
		t.Fatalf("expected newest first, got %+v", keys)
	}
}

func TestTouchAPIKeyLastUsed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	key, err := db.InsertAPIKey(ctx, sampleNewAPIKey(tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := db.TouchAPIKeyLastUsed(ctx, key.ID, now); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetAPIKeyByKeyID(ctx, key.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(now) {
		t.Fatalf("last_used_at not set correctly: %v", got.LastUsedAt)
	}
}

func TestRevokeAPIKeyIsDurableAndIdempotentGuard(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	key, err := db.InsertAPIKey(ctx, sampleNewAPIKey(tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeAPIKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetAPIKeyByKeyID(ctx, key.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RevokedAt == nil {
		t.Fatal("expected revoked_at to be set")
	}
	// Revoking again must not silently "re-revoke" (which would move the
	// timestamp) or panic - it is simply not-found-to-revoke-again.
	if err := db.RevokeAPIKey(ctx, key.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on double revoke, got %v", err)
	}
}

func TestRotateAPIKeyCreatesNewRowAndRetiresOld(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	old, err := db.InsertAPIKey(ctx, sampleNewAPIKey(tenant.ID))
	if err != nil {
		t.Fatal(err)
	}

	newKey := sampleNewAPIKey(tenant.ID)
	newKey.KeyID = "keyid00000000000000000000000099"
	newKey.SecretHash = "newhash0000000000000000000000000000000000000000000000000000"
	created, err := db.RotateAPIKey(ctx, old.ID, newKey, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if created.KeyID != newKey.KeyID || created.SecretHash != newKey.SecretHash {
		t.Fatalf("rotation did not persist the new key correctly: %+v", created)
	}

	gotOld, err := db.GetAPIKeyByKeyID(ctx, old.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOld.ExpiresAt == nil || !gotOld.ExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("expected old key to have a future grace expiry, got %v", gotOld.ExpiresAt)
	}
	if gotOld.ReplacedByID == nil || *gotOld.ReplacedByID != created.ID {
		t.Fatalf("expected old key to record replaced_by_id, got %+v", gotOld.ReplacedByID)
	}
	// Rotation must never touch the OLD secret hash: the old key is
	// still verifiable with its original secret during the grace window.
	if gotOld.SecretHash != sampleNewAPIKey(tenant.ID).SecretHash {
		t.Fatalf("rotation must not overwrite the old secret hash, got %q", gotOld.SecretHash)
	}
}

func TestRotateAPIKeyImmediateGraceExpiresNow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	old, err := db.InsertAPIKey(ctx, sampleNewAPIKey(tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	newKey := sampleNewAPIKey(tenant.ID)
	newKey.KeyID = "keyid00000000000000000000000098"

	if _, err := db.RotateAPIKey(ctx, old.ID, newKey, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	gotOld, err := db.GetAPIKeyByKeyID(ctx, old.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	// Compare against the database's own clock (via a fresh query), not
	// the test process's — the two can run on different hosts/containers
	// with a few seconds/minutes of clock skew between them, which a
	// tight host-clock bound would flake on. What actually matters is
	// "clearly not a real future expiry", not sub-second precision.
	var dbNow time.Time
	if err := db.pool.QueryRow(ctx, "SELECT now()").Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	if gotOld.ExpiresAt == nil || gotOld.ExpiresAt.After(dbNow.Add(time.Minute)) {
		t.Fatalf("expected old key's expiry to be ~now (grace=0), got %v vs db now %v", gotOld.ExpiresAt, dbNow)
	}
}

// TestRotateAPIKeyFailureLeavesOldKeyUntouched proves scenario D from the
// v0.19 spec: if the new-row half of a rotation transaction fails (here,
// forced via a scope the database CHECK constraint rejects), the OLD key
// must remain exactly as it was — no partial retirement, no dangling
// replaced_by_id.
func TestRotateAPIKeyFailureLeavesOldKeyUntouched(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	old, err := db.InsertAPIKey(ctx, sampleNewAPIKey(tenant.ID))
	if err != nil {
		t.Fatal(err)
	}

	badNewKey := sampleNewAPIKey(tenant.ID)
	badNewKey.KeyID = "keyid00000000000000000000000097"
	badNewKey.Scopes = []string{"not:a:real:scope"}
	if _, err := db.RotateAPIKey(ctx, old.ID, badNewKey, time.Now().UTC().Add(time.Hour)); err == nil {
		t.Fatal("expected rotation to fail for an invalid scope")
	}

	gotOld, err := db.GetAPIKeyByKeyID(ctx, old.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOld.ExpiresAt != nil || gotOld.ReplacedByID != nil {
		t.Fatalf("failed rotation must leave the old key completely untouched, got %+v", gotOld)
	}
	if _, err := db.GetAPIKeyByKeyID(ctx, badNewKey.KeyID); !errors.Is(err, ErrNotFound) {
		t.Fatal("the invalid new key must not have been persisted")
	}
}

func TestRotateAPIKeyUnknownOldIDIsNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	newKey := sampleNewAPIKey(tenant.ID)
	newKey.KeyID = "keyid00000000000000000000000096"
	if _, err := db.RotateAPIKey(ctx, "does-not-exist", newKey, time.Now().UTC().Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	// The new key must not have been left behind either.
	if _, err := db.GetAPIKeyByKeyID(ctx, newKey.KeyID); !errors.Is(err, ErrNotFound) {
		t.Fatal("new key must not persist when the old key lookup fails")
	}
}

// TestRotateAPIKeyTwiceSecondCallConflicts is a regression test for a
// Greptile-flagged bug: rotating an already-rotated key used to succeed
// twice, leaving two valid replacement credentials and silently
// overwriting the first replacement's replaced_by_id.
func TestRotateAPIKeyTwiceSecondCallConflicts(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	old, err := db.InsertAPIKey(ctx, sampleNewAPIKey(tenant.ID))
	if err != nil {
		t.Fatal(err)
	}

	first := sampleNewAPIKey(tenant.ID)
	first.KeyID = "keyid00000000000000000000000091"
	if _, err := db.RotateAPIKey(ctx, old.ID, first, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	second := sampleNewAPIKey(tenant.ID)
	second.KeyID = "keyid00000000000000000000000092"
	if _, err := db.RotateAPIKey(ctx, old.ID, second, time.Now().UTC().Add(time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict rotating an already-rotated key, got %v", err)
	}
	// The second rotation's key must not have been left behind.
	if _, err := db.GetAPIKeyByKeyID(ctx, second.KeyID); !errors.Is(err, ErrNotFound) {
		t.Fatal("the losing rotation's replacement key must not persist")
	}
	gotOld, err := db.GetAPIKeyByKeyID(ctx, old.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOld.ReplacedByID == nil || *gotOld.ReplacedByID == second.KeyID {
		t.Fatalf("old key's replaced_by_id must still point at the first (winning) replacement, got %+v", gotOld.ReplacedByID)
	}
}

// TestConcurrentRotationsExactlyOneWinner proves the same guarantee
// under real concurrency rather than sequential calls.
func TestConcurrentRotationsExactlyOneWinner(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	old, err := db.InsertAPIKey(ctx, sampleNewAPIKey(tenant.ID))
	if err != nil {
		t.Fatal(err)
	}

	const attempts = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			nk := sampleNewAPIKey(tenant.ID)
			nk.KeyID = fmt.Sprintf("keyidconcurrent%017d", i)
			_, err := db.RotateAPIKey(ctx, old.ID, nk, time.Now().UTC().Add(time.Hour))
			if err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, ErrConflict) {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("expected exactly 1 winning rotation among %d concurrent attempts, got %d", attempts, wins)
	}
}
