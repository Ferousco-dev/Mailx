package database

import (
	"context"
	"errors"
	"testing"
)

func sampleTemplate(tenantID, name string) NewTemplate {
	return NewTemplate{TenantID: tenantID, Name: name, Subject: "Welcome, {{name}}", Text: "Hi {{name}}"}
}

func TestCreateGetTemplate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	created, err := db.CreateTemplate(ctx, sampleTemplate(tn.ID, "welcome"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetTemplate(ctx, tn.ID, created.ID)
	if err != nil || got.Name != "welcome" || got.Subject != "Welcome, {{name}}" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestTemplateNameUniquePerTenant(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	if _, err := db.CreateTemplate(ctx, sampleTemplate(tn.ID, "dup")); err != nil {
		t.Fatal(err)
	}
	_, err := db.CreateTemplate(ctx, sampleTemplate(tn.ID, "dup"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("%v", err)
	}
	// A different tenant may reuse the same name.
	other := newTestTenant(t, db)
	if _, err := db.CreateTemplate(ctx, sampleTemplate(other.ID, "dup")); err != nil {
		t.Fatal(err)
	}
}

func TestGetTemplateCrossTenantIsNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	b := newTestTenant(t, db)
	created, _ := db.CreateTemplate(ctx, sampleTemplate(a.ID, "t"))
	if _, err := db.GetTemplate(ctx, b.ID, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestUpdateTemplateBumpsUpdatedAtAndIsPartial(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	created, _ := db.CreateTemplate(ctx, sampleTemplate(tn.ID, "t"))
	newSubject := "New subject"
	updated, err := db.UpdateTemplate(ctx, tn.ID, created.ID, TemplateUpdate{Subject: &newSubject})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Subject != newSubject || updated.Text != created.Text || updated.Name != created.Name {
		t.Fatalf("%+v", updated)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) && updated.UpdatedAt != created.UpdatedAt {
		t.Fatalf("updated_at did not advance: %v -> %v", created.UpdatedAt, updated.UpdatedAt)
	}
}

func TestUpdateTemplateCrossTenantNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	b := newTestTenant(t, db)
	created, _ := db.CreateTemplate(ctx, sampleTemplate(a.ID, "t"))
	newName := "x"
	if _, err := db.UpdateTemplate(ctx, b.ID, created.ID, TemplateUpdate{Name: &newName}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestDeleteTemplateThenGetIsNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	created, _ := db.CreateTemplate(ctx, sampleTemplate(tn.ID, "t"))
	if err := db.DeleteTemplate(ctx, tn.ID, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetTemplate(ctx, tn.ID, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if err := db.DeleteTemplate(ctx, tn.ID, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete: %v", err)
	}
}

func TestDeleteTemplateCrossTenantNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	b := newTestTenant(t, db)
	created, _ := db.CreateTemplate(ctx, sampleTemplate(a.ID, "t"))
	if err := db.DeleteTemplate(ctx, b.ID, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := db.GetTemplate(ctx, a.ID, created.ID); err != nil {
		t.Fatal("tenant a's template must survive tenant b's delete attempt")
	}
}

func TestListTemplatesKeysetPagination(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	for i := 0; i < 5; i++ {
		if _, err := db.CreateTemplate(ctx, sampleTemplate(tn.ID, "t"+string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	page1, err := db.ListTemplates(ctx, tn.ID, 3, nil)
	if err != nil || len(page1) != 3 {
		t.Fatalf("%v %v", page1, err)
	}
	last := page1[len(page1)-1]
	page2, err := db.ListTemplates(ctx, tn.ID, 3, &TemplateCursor{CreatedAt: last.CreatedAt, ID: last.ID})
	if err != nil || len(page2) != 2 {
		t.Fatalf("%v %v", page2, err)
	}
	seen := map[string]bool{}
	for _, t := range append(page1, page2...) {
		if seen[t.ID] {
			panic("duplicate across pages")
		}
		seen[t.ID] = true
	}
}

func TestListTemplatesTenantIsolated(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	b := newTestTenant(t, db)
	if _, err := db.CreateTemplate(ctx, sampleTemplate(a.ID, "t")); err != nil {
		t.Fatal(err)
	}
	got, err := db.ListTemplates(ctx, b.ID, 10, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestTemplateBodyCheckConstraint(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	if _, err := db.CreateTemplate(ctx, NewTemplate{TenantID: tn.ID, Name: "t", Subject: "s"}); err == nil {
		t.Fatal("expected the DB CHECK to reject an empty text+html template")
	}
}

func TestTemplatesMigrationRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	created, err := db.CreateTemplate(ctx, sampleTemplate(tn.ID, "t"))
	if err != nil {
		t.Fatal(err)
	}
	// Roll back past every migration newer than templates', not just one.
	var exists bool
	for {
		if err := db.MigrateDownOne(ctx); err != nil {
			t.Fatal(err)
		}
		_ = db.pool.QueryRow(ctx, `SELECT to_regclass('templates') IS NOT NULL`).Scan(&exists)
		if !exists {
			break
		}
	}
	// templates:read/write must also be gone from the scope CHECK.
	_, err = db.pool.Exec(ctx, `INSERT INTO api_keys (id, tenant_id, name, key_id, secret_hash, scopes) VALUES ('k1',$1,'n','kid','h', ARRAY['templates:read'])`, tn.ID)
	if err == nil {
		t.Fatal("templates:read scope should be rejected after downgrade")
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetTemplate(ctx, tn.ID, created.ID); err == nil {
		t.Fatal("re-up must not resurrect a row dropped with its table")
	}
	if _, err := db.CreateTemplate(ctx, sampleTemplate(tn.ID, "t2")); err != nil {
		t.Fatalf("table usable after re-up: %v", err)
	}
}
