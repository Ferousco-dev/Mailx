package database

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func mustContact(t *testing.T, db *DB, tenantID, email string) Contact {
	t.Helper()
	c, err := db.CreateContact(context.Background(), NewContact{TenantID: tenantID, Email: email})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCreateGetAudience(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	created, err := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "Newsletter"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetAudience(ctx, tn.ID, created.ID)
	if err != nil || got.Name != "Newsletter" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestAudienceDuplicateNameConflict(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	if _, err := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "dup"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "dup"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("%v", err)
	}
	other := newTestTenant(t, db)
	if _, err := db.CreateAudience(ctx, NewAudience{TenantID: other.ID, Name: "dup"}); err != nil {
		t.Fatalf("a different tenant may reuse the name: %v", err)
	}
}

func TestGetAudienceCrossTenantNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	b := newTestTenant(t, db)
	created, _ := db.CreateAudience(ctx, NewAudience{TenantID: a.ID, Name: "x"})
	if _, err := db.GetAudience(ctx, b.ID, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestUpdateAudienceRename(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	created, _ := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "old"})
	updated, err := db.UpdateAudience(ctx, tn.ID, created.ID, "new")
	if err != nil || updated.Name != "new" {
		t.Fatalf("%+v %v", updated, err)
	}
}

func TestDeleteAudienceThenGetNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	created, _ := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "x"})
	if err := db.DeleteAudience(ctx, tn.ID, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAudience(ctx, tn.ID, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestListAudiencesKeysetPaginationAndIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	other := newTestTenant(t, db)
	for i := 0; i < 5; i++ {
		db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: string(rune('a' + i))})
	}
	db.CreateAudience(ctx, NewAudience{TenantID: other.ID, Name: "z"})

	page1, err := db.ListAudiences(ctx, tn.ID, 3, nil)
	if err != nil || len(page1) != 3 {
		t.Fatalf("%v %v", page1, err)
	}
	last := page1[len(page1)-1]
	page2, err := db.ListAudiences(ctx, tn.ID, 3, &AudienceCursor{CreatedAt: last.CreatedAt, ID: last.ID})
	if err != nil || len(page2) != 2 {
		t.Fatalf("%v %v", page2, err)
	}
	for _, a := range append(page1, page2...) {
		if a.TenantID != tn.ID {
			t.Fatal("cross-tenant leak")
		}
	}
}

// -------------------------------------------------------------- membership

func TestAddAndListMembers(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	aud, _ := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "a"})
	c1 := mustContact(t, db, tn.ID, "one@example.com")
	c2 := mustContact(t, db, tn.ID, "two@example.com")
	if err := db.AddAudienceMember(ctx, tn.ID, aud.ID, c1.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.AddAudienceMember(ctx, tn.ID, aud.ID, c2.ID); err != nil {
		t.Fatal(err)
	}
	members, err := db.ListAudienceMembers(ctx, tn.ID, aud.ID, 10, nil)
	if err != nil || len(members) != 2 {
		t.Fatalf("%v %v", members, err)
	}
}

func TestAddMemberIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	aud, _ := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "a"})
	c := mustContact(t, db, tn.ID, "one@example.com")
	if err := db.AddAudienceMember(ctx, tn.ID, aud.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.AddAudienceMember(ctx, tn.ID, aud.ID, c.ID); err != nil {
		t.Fatalf("re-add must be idempotent: %v", err)
	}
	members, _ := db.ListAudienceMembers(ctx, tn.ID, aud.ID, 10, nil)
	if len(members) != 1 {
		t.Fatalf("duplicate membership row: %d", len(members))
	}
}

func TestContactInMultipleAudiences(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	a1, _ := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "a1"})
	a2, _ := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "a2"})
	c := mustContact(t, db, tn.ID, "one@example.com")
	if err := db.AddAudienceMember(ctx, tn.ID, a1.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.AddAudienceMember(ctx, tn.ID, a2.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	m1, _ := db.ListAudienceMembers(ctx, tn.ID, a1.ID, 10, nil)
	m2, _ := db.ListAudienceMembers(ctx, tn.ID, a2.ID, 10, nil)
	if len(m1) != 1 || len(m2) != 1 {
		t.Fatalf("%d %d", len(m1), len(m2))
	}
}

func TestRemoveMember(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	aud, _ := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "a"})
	c := mustContact(t, db, tn.ID, "one@example.com")
	db.AddAudienceMember(ctx, tn.ID, aud.ID, c.ID)
	if err := db.RemoveAudienceMember(ctx, tn.ID, aud.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	members, _ := db.ListAudienceMembers(ctx, tn.ID, aud.ID, 10, nil)
	if len(members) != 0 {
		t.Fatalf("%d", len(members))
	}
	if err := db.RemoveAudienceMember(ctx, tn.ID, aud.ID, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing an absent membership: %v", err)
	}
	// The contact itself must be untouched.
	if _, err := db.GetContact(ctx, tn.ID, c.ID); err != nil {
		t.Fatalf("contact affected by member removal: %v", err)
	}
}

func TestAddMemberUnknownAudienceOrContactNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	c := mustContact(t, db, tn.ID, "one@example.com")
	if err := db.AddAudienceMember(ctx, tn.ID, "nope", c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	aud, _ := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "a"})
	if err := db.AddAudienceMember(ctx, tn.ID, aud.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

// ---------------------------------------------------------- tenant integrity

func TestCannotAddAnotherTenantsContact(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	b := newTestTenant(t, db)
	aud, _ := db.CreateAudience(ctx, NewAudience{TenantID: a.ID, Name: "x"})
	victimContact := mustContact(t, db, b.ID, "victim@example.com")
	if err := db.AddAudienceMember(ctx, a.ID, aud.ID, victimContact.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant add: %v", err)
	}
}

func TestCannotAddToAnotherTenantsAudience(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	b := newTestTenant(t, db)
	victimAudience, _ := db.CreateAudience(ctx, NewAudience{TenantID: b.ID, Name: "x"})
	c := mustContact(t, db, a.ID, "one@example.com")
	if err := db.AddAudienceMember(ctx, a.ID, victimAudience.ID, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestListMembersUnknownAudienceCrossTenantNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	b := newTestTenant(t, db)
	aud, _ := db.CreateAudience(ctx, NewAudience{TenantID: a.ID, Name: "x"})
	if _, err := db.ListAudienceMembers(ctx, b.ID, aud.ID, 10, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

// ------------------------------------------------------------ lifecycle/cascade

func TestDeleteAudiencePreservesContactsAndSuppression(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	aud, _ := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "a"})
	c1 := mustContact(t, db, tn.ID, "one@example.com")
	c2 := mustContact(t, db, tn.ID, "two@example.com")
	db.AddAudienceMember(ctx, tn.ID, aud.ID, c1.ID)
	db.AddAudienceMember(ctx, tn.ID, aud.ID, c2.ID)
	sup, _, err := db.CreateSuppression(ctx, NewSuppression{TenantID: tn.ID, Email: "one@example.com", Reason: "manual", Source: "api"})
	if err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteAudience(ctx, tn.ID, aud.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetContact(ctx, tn.ID, c1.ID); err != nil {
		t.Fatalf("contact deleted with audience: %v", err)
	}
	if _, err := db.GetContact(ctx, tn.ID, c2.ID); err != nil {
		t.Fatalf("contact deleted with audience: %v", err)
	}
	if _, err := db.GetSuppression(ctx, tn.ID, sup.ID); err != nil {
		t.Fatalf("suppression affected by audience delete: %v", err)
	}
	var n int
	db.pool.QueryRow(ctx, `SELECT count(*) FROM audience_members WHERE audience_id = $1`, aud.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d orphan membership rows", n)
	}
}

func TestDeleteContactRemovesMembershipsPreservesAudiencesAndSuppression(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	a1, _ := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "a1"})
	a2, _ := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "a2"})
	c := mustContact(t, db, tn.ID, "one@example.com")
	db.AddAudienceMember(ctx, tn.ID, a1.ID, c.ID)
	db.AddAudienceMember(ctx, tn.ID, a2.ID, c.ID)
	sup, _, err := db.CreateSuppression(ctx, NewSuppression{TenantID: tn.ID, Email: "one@example.com", Reason: "manual", Source: "api"})
	if err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteContact(ctx, tn.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAudience(ctx, tn.ID, a1.ID); err != nil {
		t.Fatalf("audience deleted with contact: %v", err)
	}
	if _, err := db.GetAudience(ctx, tn.ID, a2.ID); err != nil {
		t.Fatalf("audience deleted with contact: %v", err)
	}
	if _, err := db.GetSuppression(ctx, tn.ID, sup.ID); err != nil {
		t.Fatalf("suppression affected by contact delete: %v", err)
	}
	var n int
	db.pool.QueryRow(ctx, `SELECT count(*) FROM audience_members WHERE contact_id = $1`, c.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d orphan membership rows after contact delete", n)
	}
}

func TestSuppressedContactCanJoinAudience(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	if _, _, err := db.CreateSuppression(ctx, NewSuppression{TenantID: tn.ID, Email: "one@example.com", Reason: "manual", Source: "api"}); err != nil {
		t.Fatal(err)
	}
	c := mustContact(t, db, tn.ID, "one@example.com")
	aud, _ := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "a"})
	if err := db.AddAudienceMember(ctx, tn.ID, aud.ID, c.ID); err != nil {
		t.Fatalf("suppressed contact must be addable: %v", err)
	}
}

// ---------------------------------------------------------------- concurrency

func TestConcurrentDuplicateMemberAddOneRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	aud, _ := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "a"})
	c := mustContact(t, db, tn.ID, "one@example.com")
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := db.AddAudienceMember(ctx, tn.ID, aud.ID, c.ID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var n int
	db.pool.QueryRow(ctx, `SELECT count(*) FROM audience_members WHERE audience_id = $1 AND contact_id = $2`, aud.ID, c.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
}

func TestAudiencesMigrationRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	aud, err := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	var exists bool
	for {
		if err := db.MigrateDownOne(ctx); err != nil {
			t.Fatal(err)
		}
		_ = db.pool.QueryRow(ctx, `SELECT to_regclass('audiences') IS NOT NULL`).Scan(&exists)
		if !exists {
			break
		}
	}
	_, err = db.pool.Exec(ctx, `INSERT INTO api_keys (id, tenant_id, name, key_id, secret_hash, scopes) VALUES ('k1',$1,'n','kid','h', ARRAY['audiences:read'])`, tn.ID)
	if err == nil {
		t.Fatal("audiences:read scope should be rejected after downgrade")
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAudience(ctx, tn.ID, aud.ID); err == nil {
		t.Fatal("re-up must not resurrect a row dropped with its table")
	}
	if _, err := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "b"}); err != nil {
		t.Fatalf("table usable after re-up: %v", err)
	}
}
