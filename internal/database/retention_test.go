package database

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSetTenantRetentionAndGetTenant(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)

	got, err := db.GetTenant(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RetentionDays != nil {
		t.Fatalf("expected nil retention_days for a fresh tenant, got %v", *got.RetentionDays)
	}

	days := 30
	if err := db.SetTenantRetention(ctx, tn.ID, &days); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetTenant(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RetentionDays == nil || *got.RetentionDays != 30 {
		t.Fatalf("expected retention_days=30, got %v", got.RetentionDays)
	}

	if err := db.SetTenantRetention(ctx, tn.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetTenant(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RetentionDays != nil {
		t.Fatalf("expected retention_days reset to nil, got %v", *got.RetentionDays)
	}
}

func TestSetTenantRetentionRejectsNonPositive(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)

	zero := 0
	if err := db.SetTenantRetention(ctx, tn.ID, &zero); err == nil {
		t.Fatal("expected an error for retention_days=0")
	}
	neg := -5
	if err := db.SetTenantRetention(ctx, tn.ID, &neg); err == nil {
		t.Fatal("expected an error for a negative retention_days")
	}
}

func TestSetTenantRetentionUnknownTenantIsNotFound(t *testing.T) {
	db := newTestDB(t)
	days := 30
	if err := db.SetTenantRetention(context.Background(), "does-not-exist", &days); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// backdateMessage sets a message's created_at directly - the only way to
// exercise retention purging without waiting real days.
func backdateMessage(t *testing.T, db *DB, messageID string, at time.Time) {
	t.Helper()
	if _, err := db.pool.Exec(context.Background(), `UPDATE messages SET created_at = $1 WHERE id = $2`, at, messageID); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeExpiredMessagesRespectsPerTenantRetention(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	strict := newTestTenant(t, db)  // 1-day retention: old message purged
	lenient := newTestTenant(t, db) // default (90-day) retention: old-but-within-90-days survives

	one := 1
	if err := db.SetTenantRetention(ctx, strict.ID, &one); err != nil {
		t.Fatal(err)
	}

	strictMsg, err := db.InsertMessage(ctx, sampleNewMessage(t, strict.ID))
	if err != nil {
		t.Fatal(err)
	}
	lenientMsg, err := db.InsertMessage(ctx, sampleNewMessage(t, lenient.ID))
	if err != nil {
		t.Fatal(err)
	}

	tenDaysAgo := time.Now().UTC().Add(-10 * 24 * time.Hour)
	backdateMessage(t, db, strictMsg.ID, tenDaysAgo)  // 10 days old, past the tenant's 1-day window
	backdateMessage(t, db, lenientMsg.ID, tenDaysAgo) // 10 days old, well within the 90-day default

	purged, err := db.PurgeExpiredMessages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 1 || purged[0] != strictMsg.ID {
		t.Fatalf("expected only %s purged, got %v", strictMsg.ID, purged)
	}

	if _, err := db.GetMessage(ctx, strict.ID, strictMsg.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected strict tenant's message to be gone, got %v", err)
	}
	if _, err := db.GetMessage(ctx, lenient.ID, lenientMsg.ID); err != nil {
		t.Fatalf("expected lenient tenant's message to survive: %v", err)
	}
}

func TestPurgeExpiredMessagesCascadesRelatedRows(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)

	one := 1
	if err := db.SetTenantRetention(ctx, tn.ID, &one); err != nil {
		t.Fatal(err)
	}
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	if err != nil {
		t.Fatal(err)
	}
	insertEvent(t, db, tn.ID, msg.ID, EventQueued, time.Now().UTC())
	backdateMessage(t, db, msg.ID, time.Now().UTC().Add(-48*time.Hour))

	if _, err := db.PurgeExpiredMessages(ctx); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM recipients WHERE message_id = $1`, msg.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected recipients to cascade-delete, found %d", count)
	}
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE message_id = $1`, msg.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected events to cascade-delete, found %d", count)
	}
}

func TestPurgeExpiredMessagesDetachesBroadcastRecipients(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)

	one := 1
	if err := db.SetTenantRetention(ctx, tn.ID, &one); err != nil {
		t.Fatal(err)
	}
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	if err != nil {
		t.Fatal(err)
	}
	backdateMessage(t, db, msg.ID, time.Now().UTC().Add(-48*time.Hour))

	aud, err := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "purge-test"})
	if err != nil {
		t.Fatal(err)
	}
	contact, err := db.CreateContact(ctx, NewContact{TenantID: tn.ID, Email: "bcast@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := db.CreateTemplate(ctx, NewTemplate{TenantID: tn.ID, Name: "purge-tmpl", Subject: "hi", HTML: "<p>hi</p>"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateBroadcast(ctx, NewBroadcast{TenantID: tn.ID, Name: "purge-broadcast", AudienceID: aud.ID, TemplateID: tmpl.ID, FromAddress: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	brID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx,
		`INSERT INTO broadcast_recipients (id, broadcast_id, contact_id, tenant_id, email, status, message_id) VALUES ($1,$2,$3,$4,$5,'materialized',$6)`,
		brID, b.ID, contact.ID, tn.ID, contact.Email, msg.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := db.PurgeExpiredMessages(ctx); err != nil {
		t.Fatal(err)
	}

	var messageID *string
	if err := db.pool.QueryRow(ctx, `SELECT message_id FROM broadcast_recipients WHERE broadcast_id = $1 AND contact_id = $2`, b.ID, contact.ID).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	if messageID != nil {
		t.Fatalf("expected broadcast_recipients.message_id nulled out, got %v", *messageID)
	}
}
