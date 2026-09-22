package database

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func sampleContact(tenantID, email string) NewContact {
	return NewContact{TenantID: tenantID, Email: email, Name: "Alice", Attributes: map[string]string{"plan": "pro"}}
}

func TestCreateGetContact(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	created, err := db.CreateContact(ctx, sampleContact(tn.ID, "alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetContact(ctx, tn.ID, created.ID)
	if err != nil || got.Email != "alice@example.com" || got.Attributes["plan"] != "pro" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestContactDuplicateEmailConflict(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	if _, err := db.CreateContact(ctx, sampleContact(tn.ID, "dup@example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateContact(ctx, sampleContact(tn.ID, "  <dup@EXAMPLE.COM>  ")); !errors.Is(err, ErrConflict) {
		t.Fatalf("%v", err)
	}
}

// Local-part case is preserved for identity: these are two DIFFERENT contacts.
func TestContactLocalPartCaseNotFolded(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	if _, err := db.CreateContact(ctx, sampleContact(tn.ID, "Alice@example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateContact(ctx, sampleContact(tn.ID, "alice@example.com")); err != nil {
		t.Fatalf("case-distinct local part must be a separate contact: %v", err)
	}
}

func TestContactSameEmailAcrossTenantsBothSucceed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	b := newTestTenant(t, db)
	if _, err := db.CreateContact(ctx, sampleContact(a.ID, "alice@example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateContact(ctx, sampleContact(b.ID, "alice@example.com")); err != nil {
		t.Fatal(err)
	}
}

func TestGetContactCrossTenantIsNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	b := newTestTenant(t, db)
	created, _ := db.CreateContact(ctx, sampleContact(a.ID, "alice@example.com"))
	if _, err := db.GetContact(ctx, b.ID, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestUpdateContactPartial(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	created, _ := db.CreateContact(ctx, sampleContact(tn.ID, "alice@example.com"))
	newName := "Alice B"
	updated, err := db.UpdateContact(ctx, tn.ID, created.ID, ContactUpdate{Name: &newName})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Alice B" || updated.Email != created.Email {
		t.Fatalf("%+v", updated)
	}
}

func TestUpdateContactEmailChecksNewUniqueness(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	c1, _ := db.CreateContact(ctx, sampleContact(tn.ID, "alice@example.com"))
	db.CreateContact(ctx, sampleContact(tn.ID, "bob@example.com"))
	taken := "bob@example.com"
	if _, err := db.UpdateContact(ctx, tn.ID, c1.ID, ContactUpdate{Email: &taken}); !errors.Is(err, ErrConflict) {
		t.Fatalf("%v", err)
	}
}

func TestUpdateContactCrossTenantNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	b := newTestTenant(t, db)
	created, _ := db.CreateContact(ctx, sampleContact(a.ID, "alice@example.com"))
	name := "x"
	if _, err := db.UpdateContact(ctx, b.ID, created.ID, ContactUpdate{Name: &name}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestDeleteContactThenGetNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	created, _ := db.CreateContact(ctx, sampleContact(tn.ID, "alice@example.com"))
	if err := db.DeleteContact(ctx, tn.ID, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetContact(ctx, tn.ID, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestListContactsKeysetPaginationAndIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	other := newTestTenant(t, db)
	for i := 0; i < 5; i++ {
		db.CreateContact(ctx, sampleContact(tn.ID, string(rune('a'+i))+"@example.com"))
	}
	db.CreateContact(ctx, sampleContact(other.ID, "z@example.com"))

	page1, err := db.ListContacts(ctx, tn.ID, 3, nil)
	if err != nil || len(page1) != 3 {
		t.Fatalf("%v %v", page1, err)
	}
	last := page1[len(page1)-1]
	page2, err := db.ListContacts(ctx, tn.ID, 3, &ContactCursor{CreatedAt: last.CreatedAt, ID: last.ID})
	if err != nil || len(page2) != 2 {
		t.Fatalf("%v %v", page2, err)
	}
	seen := map[string]bool{}
	for _, c := range append(page1, page2...) {
		if seen[c.ID] {
			t.Fatal("duplicate across pages")
		}
		seen[c.ID] = true
		if c.TenantID != tn.ID {
			t.Fatal("cross-tenant leak")
		}
	}
}

// ---------------------------------------------------- suppression independence

func TestContactAndSuppressionAreIndependent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)

	created, err := db.CreateContact(ctx, sampleContact(tn.ID, "alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	sup, _, err := db.CreateSuppression(ctx, NewSuppression{TenantID: tn.ID, Email: "alice@example.com", Reason: "manual", Source: "api"})
	if err != nil {
		t.Fatal(err)
	}

	// Deleting the contact must not delete the suppression.
	if err := db.DeleteContact(ctx, tn.ID, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSuppression(ctx, tn.ID, sup.ID); err != nil {
		t.Fatalf("suppression deleted alongside contact: %v", err)
	}

	// Recreating the contact must not remove or reset the suppression.
	recreated, err := db.CreateContact(ctx, sampleContact(tn.ID, "alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSuppression(ctx, tn.ID, sup.ID); err != nil {
		t.Fatalf("suppression lost on contact recreate: %v", err)
	}

	// Deleting the suppression must not delete the contact.
	if err := db.DeleteSuppression(ctx, tn.ID, sup.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetContact(ctx, tn.ID, recreated.ID); err != nil {
		t.Fatalf("contact deleted alongside suppression: %v", err)
	}
}

func TestUpdateContactEmailDoesNotMigrateSuppression(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	created, _ := db.CreateContact(ctx, sampleContact(tn.ID, "alice@example.com"))
	sup, _, err := db.CreateSuppression(ctx, NewSuppression{TenantID: tn.ID, Email: "alice@example.com", Reason: "manual", Source: "api"})
	if err != nil {
		t.Fatal(err)
	}
	newEmail := "alice@newdomain.com"
	if _, err := db.UpdateContact(ctx, tn.ID, created.ID, ContactUpdate{Email: &newEmail}); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetSuppression(ctx, tn.ID, sup.ID)
	if err != nil || got.Email != "alice@example.com" {
		t.Fatalf("suppression migrated: %+v %v", got, err)
	}
}

func TestSuppressedAddressCanStillBecomeContact(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	if _, _, err := db.CreateSuppression(ctx, NewSuppression{TenantID: tn.ID, Email: "alice@example.com", Reason: "manual", Source: "api"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateContact(ctx, sampleContact(tn.ID, "alice@example.com")); err != nil {
		t.Fatalf("suppressed address must still be a valid contact: %v", err)
	}
}

// ---------------------------------------------------------------- concurrency

func TestConcurrentDuplicateContactCreateOnlyOneSucceeds(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	var wg sync.WaitGroup
	var ok, conflict int
	var mu sync.Mutex
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.CreateContact(ctx, sampleContact(tn.ID, "race@example.com"))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if errors.Is(err, ErrConflict) {
				conflict++
			}
		}()
	}
	wg.Wait()
	if ok != 1 || conflict != 9 {
		t.Fatalf("ok=%d conflict=%d, want 1/9", ok, conflict)
	}
}

func TestContactsMigrationRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	created, err := db.CreateContact(ctx, sampleContact(tn.ID, "alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	// Roll back past every migration newer than contacts', not just one
	// (audiences' FK to contacts must also come down first).
	var exists bool
	for {
		if err := db.MigrateDownOne(ctx); err != nil {
			t.Fatal(err)
		}
		_ = db.pool.QueryRow(ctx, `SELECT to_regclass('contacts') IS NOT NULL`).Scan(&exists)
		if !exists {
			break
		}
	}
	if exists {
		t.Fatal("down migration must drop contacts")
	}
	_, err = db.pool.Exec(ctx, `INSERT INTO api_keys (id, tenant_id, name, key_id, secret_hash, scopes) VALUES ('k1',$1,'n','kid','h', ARRAY['contacts:read'])`, tn.ID)
	if err == nil {
		t.Fatal("contacts:read scope should be rejected after downgrade")
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetContact(ctx, tn.ID, created.ID); err == nil {
		t.Fatal("re-up must not resurrect a row dropped with its table")
	}
	if _, err := db.CreateContact(ctx, sampleContact(tn.ID, "bob@example.com")); err != nil {
		t.Fatalf("table usable after re-up: %v", err)
	}
}
