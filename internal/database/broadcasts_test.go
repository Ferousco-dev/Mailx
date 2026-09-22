package database

import (
	"context"
	"errors"
	"testing"
	"time"
)

func sampleBroadcast(tenantID, audienceID, templateID string) NewBroadcast {
	return NewBroadcast{TenantID: tenantID, AudienceID: audienceID, TemplateID: templateID, Name: "Sept Update",
		FromAddress: "a@example.com", SubjectTemplate: "Hi {{name}}", TextTemplate: "Body {{name}}"}
}

func TestCreateGetBroadcast(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	aud, _ := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "a"})
	tmpl, _ := db.CreateTemplate(ctx, NewTemplate{TenantID: tn.ID, Name: "t", Subject: "s", Text: "t"})
	b, err := db.CreateBroadcast(ctx, sampleBroadcast(tn.ID, aud.ID, tmpl.ID))
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "accepted" || b.SnapshotComplete {
		t.Fatalf("%+v", b)
	}
	got, err := db.GetBroadcast(ctx, tn.ID, b.ID)
	if err != nil || got.Name != "Sept Update" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestGetBroadcastCrossTenantNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	b := newTestTenant(t, db)
	aud, _ := db.CreateAudience(ctx, NewAudience{TenantID: a.ID, Name: "a"})
	tmpl, _ := db.CreateTemplate(ctx, NewTemplate{TenantID: a.ID, Name: "t", Subject: "s", Text: "t"})
	created, _ := db.CreateBroadcast(ctx, sampleBroadcast(a.ID, aud.ID, tmpl.ID))
	if _, err := db.GetBroadcast(ctx, b.ID, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestListBroadcastsKeysetPaginationAndIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	other := newTestTenant(t, db)
	aud, _ := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "a"})
	tmpl, _ := db.CreateTemplate(ctx, NewTemplate{TenantID: tn.ID, Name: "t", Subject: "s", Text: "t"})
	for i := 0; i < 5; i++ {
		db.CreateBroadcast(ctx, sampleBroadcast(tn.ID, aud.ID, tmpl.ID))
	}
	otherAud, _ := db.CreateAudience(ctx, NewAudience{TenantID: other.ID, Name: "a"})
	otherTmpl, _ := db.CreateTemplate(ctx, NewTemplate{TenantID: other.ID, Name: "t", Subject: "s", Text: "t"})
	db.CreateBroadcast(ctx, sampleBroadcast(other.ID, otherAud.ID, otherTmpl.ID))

	page1, err := db.ListBroadcasts(ctx, tn.ID, 3, nil)
	if err != nil || len(page1) != 3 {
		t.Fatalf("%v %v", page1, err)
	}
	last := page1[len(page1)-1]
	page2, err := db.ListBroadcasts(ctx, tn.ID, 3, &BroadcastCursor{CreatedAt: last.CreatedAt, ID: last.ID})
	if err != nil || len(page2) != 2 {
		t.Fatalf("%v %v", page2, err)
	}
	for _, b := range append(page1, page2...) {
		if b.TenantID != tn.ID {
			t.Fatal("cross-tenant leak")
		}
	}
}

func TestCreateBroadcastIdempotencyCompletion(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	aud, _ := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "a"})
	tmpl, _ := db.CreateTemplate(ctx, NewTemplate{TenantID: tn.ID, Name: "t", Subject: "s", Text: "t"})
	now := time.Now().UTC()
	exp, stale := now.Add(time.Hour), now.Add(-time.Minute)
	claim, owned, err := db.ClaimIdempotencyKey(ctx, tn.ID, "broadcasts.create", "k1", "fp1", exp, stale)
	if err != nil || !owned {
		t.Fatalf("%v %v", err, owned)
	}
	in := sampleBroadcast(tn.ID, aud.ID, tmpl.ID)
	in.IdempotencyCompletion = &IdempotencyCompletion{Operation: "broadcasts.create", IdempotencyKey: "k1", Fingerprint: "fp1"}
	b, err := db.CreateBroadcast(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	claim, err = db.GetIdempotencyKey(ctx, tn.ID, "broadcasts.create", "k1")
	if err != nil || claim.Status != IdempotencyCompleted || claim.ResourceID == nil || *claim.ResourceID != b.ID {
		t.Fatalf("%+v %v", claim, err)
	}
}

func TestCreateBroadcastIdempotencyLostRaceIsConflict(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	aud, _ := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "a"})
	tmpl, _ := db.CreateTemplate(ctx, NewTemplate{TenantID: tn.ID, Name: "t", Subject: "s", Text: "t"})
	in := sampleBroadcast(tn.ID, aud.ID, tmpl.ID)
	// No claim ever taken for this key: completion must fail because no
	// in_progress row exists to complete.
	in.IdempotencyCompletion = &IdempotencyCompletion{Operation: "broadcasts.create", IdempotencyKey: "missing", Fingerprint: "fp"}
	if _, err := db.CreateBroadcast(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("%v", err)
	}
}
