package database

import (
	"context"
	"testing"
	"time"
)

func TestCreateHumanDuplicateEmailConflicts(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.CreateHuman(ctx, "Ada", "Ada@Example.com", "hash1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateHuman(ctx, "Other Ada", "ada@example.com", "hash2"); err == nil {
		t.Fatal("expected conflict on case-insensitive duplicate email")
	}
}

func TestGetHumanByEmailCaseInsensitive(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	h, err := db.CreateHuman(ctx, "Ada", "ada@example.com", "hash1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetHumanByEmail(ctx, "ADA@EXAMPLE.COM")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != h.ID {
		t.Fatalf("expected same human, got %q vs %q", got.ID, h.ID)
	}
}

func TestRefreshTokenLifecycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	h, err := db.CreateHuman(ctx, "Ada", "ada@example.com", "hash1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	tok, err := db.CreateRefreshToken(ctx, h.ID, "hash-of-raw", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetRefreshTokenByHash(ctx, "hash-of-raw")
	if err != nil {
		t.Fatal(err)
	}
	if got.RevokedAt != nil {
		t.Fatal("expected not revoked")
	}
	revoked, err := db.RevokeRefreshToken(ctx, tok.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("expected RevokeRefreshToken to report it revoked the row")
	}
	got, err = db.GetRefreshTokenByHash(ctx, "hash-of-raw")
	if err != nil {
		t.Fatal(err)
	}
	if got.RevokedAt == nil {
		t.Fatal("expected revoked")
	}

	revokedAgain, err := db.RevokeRefreshToken(ctx, tok.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if revokedAgain {
		t.Fatal("expected a second revoke of the same already-revoked token to report false (the race-safety signal)")
	}
}

func TestRevokeAllRefreshTokensForHuman(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	h, err := db.CreateHuman(ctx, "Ada", "ada@example.com", "hash1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := db.CreateRefreshToken(ctx, h.ID, "hash-a", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateRefreshToken(ctx, h.ID, "hash-b", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeAllRefreshTokensForHuman(ctx, h.ID, now); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{"hash-a", "hash-b"} {
		got, err := db.GetRefreshTokenByHash(ctx, hash)
		if err != nil {
			t.Fatal(err)
		}
		if got.RevokedAt == nil {
			t.Fatalf("expected %s revoked", hash)
		}
	}
}

func TestCreateOrganizationAtomic(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	h, err := db.CreateHuman(ctx, "Ada", "ada@example.com", "hash1")
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := db.CreateOrganization(ctx, h.ID, "Acme Inc")
	if err != nil {
		t.Fatal(err)
	}
	orgs, err := db.ListOrganizationsForHuman(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(orgs) != 1 || orgs[0].ID != tenant.ID {
		t.Fatalf("expected exactly the created org, got %+v", orgs)
	}
}

func TestCreateOrganizationFailurePartwayLeavesNoOrphan(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	// A nonexistent human_id makes the tenant_members insert violate its FK,
	// so the whole transaction must roll back — no orphaned tenant row.
	_, err := db.CreateOrganization(ctx, "nonexistent-human-id", "Acme Inc")
	if err == nil {
		t.Fatal("expected an error for a nonexistent human_id")
	}
	var count int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE name = 'Acme Inc'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected no orphaned tenant row, found %d", count)
	}
}

func TestListOrganizationsForHumanOnlyReturnsMemberships(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	h1, err := db.CreateHuman(ctx, "Ada", "ada@example.com", "hash1")
	if err != nil {
		t.Fatal(err)
	}
	h2, err := db.CreateHuman(ctx, "Bea", "bea@example.com", "hash2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateOrganization(ctx, h1.ID, "Ada's Org"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateOrganization(ctx, h2.ID, "Bea's Org"); err != nil {
		t.Fatal(err)
	}
	orgs, err := db.ListOrganizationsForHuman(ctx, h1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(orgs) != 1 || orgs[0].Name != "Ada's Org" {
		t.Fatalf("expected only Ada's org, got %+v", orgs)
	}
}

// TestTouchHumanLoginIsMonotonic proves the fix for the race Greptile
// flagged: an out-of-order call (an earlier wall-clock timestamp arriving
// AFTER a later one was already recorded) must not move last_login_at
// backwards.
func TestTouchHumanLoginIsMonotonic(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	h, err := db.CreateHuman(ctx, "Ada", "ada-monotonic@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}

	later := time.Now().UTC().Truncate(time.Microsecond)
	earlier := later.Add(-time.Minute)

	if err := db.TouchHumanLogin(ctx, h.ID, later); err != nil {
		t.Fatal(err)
	}
	if err := db.TouchHumanLogin(ctx, h.ID, earlier); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetHuman(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastLoginAt == nil || !got.LastLoginAt.Equal(later) {
		t.Fatalf("expected last_login_at to stay at the later timestamp %v, got %v", later, got.LastLoginAt)
	}
}

// TestTouchLoginAndCreateRefreshTokenIsAtomicAndMonotonic exercises the
// combined method Login actually uses.
func TestTouchLoginAndCreateRefreshTokenIsAtomicAndMonotonic(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	h, err := db.CreateHuman(ctx, "Ada", "ada-atomic@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}

	first := time.Now().UTC().Truncate(time.Microsecond)
	tok, actual, err := db.TouchLoginAndCreateRefreshToken(ctx, h.ID, first, "hash1", first.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if tok.ID == "" {
		t.Fatal("expected a created refresh token")
	}
	if !actual.Equal(first) {
		t.Fatalf("expected the returned actual last_login_at to be %v, got %v", first, actual)
	}
	got, err := db.GetHuman(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastLoginAt == nil || !got.LastLoginAt.Equal(first) {
		t.Fatalf("expected last_login_at set to %v, got %v", first, got.LastLoginAt)
	}

	// An out-of-order (earlier) call must lose the monotonic guard AND
	// report the true stored value back, not its own earlier timestamp -
	// otherwise a caller building a response from the return value alone
	// would tell the client a last-login time older than what is actually
	// stored (Greptile P2, PR #22).
	earlier := first.Add(-time.Minute)
	_, actual2, err := db.TouchLoginAndCreateRefreshToken(ctx, h.ID, earlier, "hash2", earlier.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !actual2.Equal(first) {
		t.Fatalf("expected the lost-race call to report the true stored value %v, got %v", first, actual2)
	}
	got, err = db.GetHuman(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastLoginAt.Equal(first) {
		t.Fatalf("expected an out-of-order call to leave last_login_at at %v, got %v", first, got.LastLoginAt)
	}
}
