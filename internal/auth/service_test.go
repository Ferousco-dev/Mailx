package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestServiceCreateAndAuthenticate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	svc := NewService(db, nil)

	gen, record, err := svc.Create(ctx, tenant.ID, "prod key", []string{string(ScopeEmailsSend)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gen.Raw == "" || record.ID == "" {
		t.Fatalf("unexpected create result: %+v %+v", gen, record)
	}

	authed, err := svc.Authenticate(ctx, gen.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if authed.TenantID != tenant.ID {
		t.Fatalf("expected tenant %s, got %s", tenant.ID, authed.TenantID)
	}
	if authed.LastUsedAt == nil {
		t.Fatal("expected first authentication to set last_used_at")
	}
}

func TestServiceCreateRejectsUnknownScope(t *testing.T) {
	db := newTestDB(t)
	svc := NewService(db, nil)
	tenant := newTestTenant(t, db)
	if _, _, err := svc.Create(context.Background(), tenant.ID, "x", []string{"not:a:scope"}, nil); err == nil {
		t.Fatal("expected unknown scope to be rejected before any DB write")
	}
}

func TestServiceAuthenticateUnknownKey(t *testing.T) {
	db := newTestDB(t)
	svc := NewService(db, nil)
	unknown := "mx_" + strings.Repeat("0", keyIDHexLen) + "_" + strings.Repeat("0", secretHexLen)
	if _, err := svc.Authenticate(context.Background(), unknown); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("expected ErrUnknownKey, got %v", err)
	}
}

func TestServiceAuthenticateMalformedCredential(t *testing.T) {
	db := newTestDB(t)
	svc := NewService(db, nil)
	for _, raw := range []string{"", "not-a-key", "Bearer mx_abc", "mx_"} {
		if _, err := svc.Authenticate(context.Background(), raw); !errors.Is(err, ErrUnknownKey) {
			t.Errorf("input %q: expected ErrUnknownKey (malformed collapses to the same class), got %v", raw, err)
		}
	}
}

func TestServiceAuthenticateWrongSecret(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	svc := NewService(db, nil)
	gen, _, err := svc.Create(ctx, tenant.ID, "x", []string{string(ScopeEmailsRead)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tampered := gen.Raw[:len(gen.Raw)-1] + "0"
	if tampered == gen.Raw {
		tampered = gen.Raw[:len(gen.Raw)-1] + "1"
	}
	if _, err := svc.Authenticate(ctx, tampered); !errors.Is(err, ErrWrongSecret) {
		t.Fatalf("expected ErrWrongSecret, got %v", err)
	}
}

func TestServiceAuthenticateRevoked(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	svc := NewService(db, nil)
	gen, record, err := svc.Create(ctx, tenant.ID, "x", []string{string(ScopeEmailsRead)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Revoke(ctx, record.KeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, gen.Raw); !errors.Is(err, ErrRevoked) {
		t.Fatalf("expected ErrRevoked, got %v", err)
	}
}

func TestServiceAuthenticateExpirationBoundary(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	svc := NewService(db, nil)

	frozen := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return frozen }
	ttl := time.Hour
	gen, _, err := svc.Create(ctx, tenant.ID, "x", []string{string(ScopeEmailsRead)}, &ttl)
	if err != nil {
		t.Fatal(err)
	}

	svc.now = func() time.Time { return frozen.Add(59 * time.Minute) }
	if _, err := svc.Authenticate(ctx, gen.Raw); err != nil {
		t.Fatalf("before expiry: expected success, got %v", err)
	}

	svc.now = func() time.Time { return frozen.Add(time.Hour) }
	if _, err := svc.Authenticate(ctx, gen.Raw); !errors.Is(err, ErrExpired) {
		t.Fatalf("exactly at expiry: expected ErrExpired, got %v", err)
	}

	svc.now = func() time.Time { return frozen.Add(61 * time.Minute) }
	if _, err := svc.Authenticate(ctx, gen.Raw); !errors.Is(err, ErrExpired) {
		t.Fatalf("after expiry: expected ErrExpired, got %v", err)
	}
}

func TestServiceLastUsedThrottling(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	svc := NewService(db, nil)
	frozen := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return frozen }

	gen, _, err := svc.Create(ctx, tenant.ID, "x", []string{string(ScopeEmailsRead)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := svc.Authenticate(ctx, gen.Raw)
	if err != nil {
		t.Fatal(err)
	}

	// Authenticating again a moment later (well within touchThreshold)
	// must NOT move last_used_at — that is the throttling this test
	// exists to prove.
	svc.now = func() time.Time { return frozen.Add(time.Minute) }
	second, err := svc.Authenticate(ctx, gen.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if !second.LastUsedAt.Equal(*first.LastUsedAt) {
		t.Fatalf("expected last_used_at to stay throttled, got %v then %v", first.LastUsedAt, second.LastUsedAt)
	}

	// Past the threshold, it must advance.
	svc.now = func() time.Time { return frozen.Add(touchThreshold + time.Minute) }
	third, err := svc.Authenticate(ctx, gen.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if !third.LastUsedAt.After(*second.LastUsedAt) {
		t.Fatalf("expected last_used_at to advance past the threshold, got %v then %v", second.LastUsedAt, third.LastUsedAt)
	}
}

func TestServiceFailedAuthenticationNeverTouchesLastUsed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	svc := NewService(db, nil)
	gen, record, err := svc.Create(ctx, tenant.ID, "x", []string{string(ScopeEmailsRead)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = gen
	tampered := "mx_" + record.KeyID + "_" + strings.Repeat("0", secretHexLen)
	if _, err := svc.Authenticate(ctx, tampered); !errors.Is(err, ErrWrongSecret) {
		t.Fatalf("expected ErrWrongSecret, got %v", err)
	}
	keys, err := svc.List(ctx, tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].LastUsedAt != nil {
		t.Fatalf("a failed authentication must not set last_used_at, got %+v", keys)
	}
}

func TestServiceRotateGraceAndImmediate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	svc := NewService(db, nil)
	frozen := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return frozen }

	oldGen, oldRecord, err := svc.Create(ctx, tenant.ID, "prod", []string{string(ScopeEmailsSend), string(ScopeEmailsRead)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, oldGen.Raw); err != nil {
		t.Fatal(err)
	}

	newGen, newRecord, err := svc.Rotate(ctx, oldRecord.KeyID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if newRecord.KeyID == oldRecord.KeyID {
		t.Fatal("rotation must produce a different key id")
	}
	if len(newRecord.Scopes) != 2 {
		t.Fatalf("expected rotated key to inherit scopes, got %v", newRecord.Scopes)
	}

	// New key works immediately.
	if _, err := svc.Authenticate(ctx, newGen.Raw); err != nil {
		t.Fatalf("new key should authenticate immediately: %v", err)
	}
	// Old key still works during the grace window.
	svc.now = func() time.Time { return frozen.Add(30 * time.Minute) }
	if _, err := svc.Authenticate(ctx, oldGen.Raw); err != nil {
		t.Fatalf("old key should still work during grace: %v", err)
	}
	// After grace, old key is rejected but new key still works.
	svc.now = func() time.Time { return frozen.Add(2 * time.Hour) }
	if _, err := svc.Authenticate(ctx, oldGen.Raw); !errors.Is(err, ErrExpired) {
		t.Fatalf("expected old key to be expired after grace, got %v", err)
	}
	if _, err := svc.Authenticate(ctx, newGen.Raw); err != nil {
		t.Fatalf("new key must still work after old key's grace expires: %v", err)
	}
}

func TestServiceRotateImmediateInvalidatesOldKeyRightAway(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	svc := NewService(db, nil)

	oldGen, oldRecord, err := svc.Create(ctx, tenant.ID, "x", []string{string(ScopeEmailsRead)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	newGen, _, err := svc.Rotate(ctx, oldRecord.KeyID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, newGen.Raw); err != nil {
		t.Fatalf("new key should work: %v", err)
	}
	if _, err := svc.Authenticate(ctx, oldGen.Raw); !errors.Is(err, ErrExpired) {
		t.Fatalf("expected the old key to be rejected immediately after zero-grace rotation, got %v", err)
	}
}

func TestServiceRevokeIsImmediateEvenDuringWouldBeGrace(t *testing.T) {
	// A revoked key must not get a grace period merely because rotation
	// normally supports one — revocation and rotation are different
	// operations with different urgency (compromise vs planned change).
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	svc := NewService(db, nil)

	gen, record, err := svc.Create(ctx, tenant.ID, "x", []string{string(ScopeEmailsRead)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Rotate(ctx, record.KeyID, time.Hour); err != nil {
		t.Fatal(err)
	}
	// Even though rotation gave it an hour of grace, an explicit revoke
	// must win immediately.
	if err := svc.Revoke(ctx, record.KeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, gen.Raw); !errors.Is(err, ErrRevoked) {
		t.Fatalf("expected ErrRevoked to override any remaining grace period, got %v", err)
	}
}

func TestServiceTenantIsolationAcrossKeys(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenantA := newTestTenant(t, db)
	tenantB, err := db.CreateTenant(ctx, "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(db, nil)

	genA, _, err := svc.Create(ctx, tenantA.ID, "a", []string{string(ScopeEmailsSend)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	genB, _, err := svc.Create(ctx, tenantB.ID, "b", []string{string(ScopeEmailsSend)}, nil)
	if err != nil {
		t.Fatal(err)
	}

	authedA, err := svc.Authenticate(ctx, genA.Raw)
	if err != nil {
		t.Fatal(err)
	}
	authedB, err := svc.Authenticate(ctx, genB.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if authedA.TenantID != tenantA.ID || authedB.TenantID != tenantB.ID {
		t.Fatalf("keys resolved to the wrong tenant: %s / %s", authedA.TenantID, authedB.TenantID)
	}
	if authedA.TenantID == authedB.TenantID {
		t.Fatal("distinct tenants must not collapse to the same id")
	}
}

// TestServiceRotateRejectsExpiredKey is a regression test for a
// Greptile-flagged bug: rotating an already-expired key used to copy its
// past ExpiresAt to the replacement (an immediately-dead new credential)
// and, with a positive grace period, push the OLD key's expiry into the
// future — reviving an already-expired secret instead of retiring it.
func TestServiceRotateRejectsExpiredKey(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	svc := NewService(db, nil)
	frozen := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return frozen }

	ttl := time.Hour
	_, record, err := svc.Create(ctx, tenant.ID, "x", []string{string(ScopeEmailsRead)}, &ttl)
	if err != nil {
		t.Fatal(err)
	}

	svc.now = func() time.Time { return frozen.Add(2 * time.Hour) } // now past expiry
	_, _, err = svc.Rotate(ctx, record.KeyID, time.Hour)
	if err == nil {
		t.Fatal("expected rotating an already-expired key to be rejected, not revive it")
	}
	// Wrapped in ErrExpired (CodeRabbit, PR #28) so a caller like the HTTP
	// rotate handler can map this to 409 instead of a generic 500 - it
	// used to be a bare fmt.Errorf with no matchable sentinel.
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("expected err to wrap ErrExpired, got %v", err)
	}
}

func TestServiceRotateRejectsRevokedKey(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	svc := NewService(db, nil)
	_, record, err := svc.Create(ctx, tenant.ID, "x", []string{string(ScopeEmailsRead)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Revoke(ctx, record.KeyID); err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.Rotate(ctx, record.KeyID, time.Hour)
	if err == nil {
		t.Fatal("expected rotating a revoked key to be rejected")
	}
	// Wrapped in ErrRevoked (CodeRabbit, PR #28) for the same reason as
	// ErrExpired above: this is exactly the race where a key is revoked
	// concurrently between the HTTP handler's own unlocked pre-check and
	// this call - the handler must be able to tell this apart from a real
	// internal error.
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("expected err to wrap ErrRevoked, got %v", err)
	}
}
