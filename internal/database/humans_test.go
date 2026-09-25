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
	now := time.Now().UTC()
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
	if err := db.RevokeRefreshToken(ctx, tok.ID, now); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetRefreshTokenByHash(ctx, "hash-of-raw")
	if err != nil {
		t.Fatal(err)
	}
	if got.RevokedAt == nil {
		t.Fatal("expected revoked")
	}
}

func TestRevokeAllRefreshTokensForHuman(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	h, err := db.CreateHuman(ctx, "Ada", "ada@example.com", "hash1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
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
