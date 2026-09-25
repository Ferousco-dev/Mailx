package database

import (
	"context"
	"errors"
	"testing"
)

func TestExportSubjectDataFindsContactAudienceAndRecipients(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)

	c, err := db.CreateContact(ctx, NewContact{TenantID: tn.ID, Email: "subject@example.com", Name: "Subject Person"})
	if err != nil {
		t.Fatal(err)
	}
	aud, err := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "gdpr-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO audience_members (audience_id, contact_id, tenant_id) VALUES ($1,$2,$3)`, aud.ID, c.ID, tn.ID); err != nil {
		t.Fatal(err)
	}

	newMsg := func(addr string) NewMessage {
		id, err := newID()
		if err != nil {
			t.Fatal(err)
		}
		return NewMessage{
			ID: id, TenantID: tn.ID, MailFrom: "<sender@example.com>",
			FromHeader: "Sender <sender@example.com>", Subject: "hi",
			MessageIDHeader: "<" + id + "@example.com>",
			Recipients:      []RecipientInput{{Address: addr, HeaderKind: strPtr("to")}},
		}
	}
	if _, err := db.InsertMessage(ctx, newMsg("subject@example.com")); err != nil {
		t.Fatal(err)
	}

	data, err := db.ExportSubjectData(ctx, tn.ID, "subject@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if data.Contact == nil || data.Contact.ID != c.ID {
		t.Fatalf("expected contact %s found, got %+v", c.ID, data.Contact)
	}
	if len(data.AudienceID) != 1 || data.AudienceID[0] != aud.ID {
		t.Fatalf("expected audience membership %s, got %v", aud.ID, data.AudienceID)
	}
	if len(data.Recipients) != 1 {
		t.Fatalf("expected 1 recipient record, got %d", len(data.Recipients))
	}
}

func TestExportSubjectDataMatchesDifferentAddressForms(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)

	newMsg := func(addr string) NewMessage {
		id, err := newID()
		if err != nil {
			t.Fatal(err)
		}
		return NewMessage{
			ID: id, TenantID: tn.ID, MailFrom: "<sender@example.com>",
			FromHeader: "Sender <sender@example.com>", Subject: "hi",
			MessageIDHeader: "<" + id + "@example.com>",
			Recipients:      []RecipientInput{{Address: addr, HeaderKind: strPtr("to")}},
		}
	}
	// Same mailbox, three different stored representations - exactly what
	// RCPT TO (bracket form) vs parsed headers (bare / display-name form)
	// produce in practice.
	if _, err := db.InsertMessage(ctx, newMsg("<multi@example.com>")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertMessage(ctx, newMsg("multi@example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertMessage(ctx, newMsg("Multi Person <multi@example.com>")); err != nil {
		t.Fatal(err)
	}

	data, err := db.ExportSubjectData(ctx, tn.ID, "multi@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Recipients) != 3 {
		t.Fatalf("expected all 3 address forms matched, got %d: %+v", len(data.Recipients), data.Recipients)
	}
}

func TestExportSubjectDataNoContactStillFindsRecipients(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)

	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	msg := NewMessage{
		ID: id, TenantID: tn.ID, MailFrom: "<sender@example.com>",
		FromHeader: "Sender <sender@example.com>", Subject: "hi",
		MessageIDHeader: "<" + id + "@example.com>",
		Recipients:      []RecipientInput{{Address: "nocontact@example.com", HeaderKind: strPtr("to")}},
	}
	if _, err := db.InsertMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}

	data, err := db.ExportSubjectData(ctx, tn.ID, "nocontact@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if data.Contact != nil {
		t.Fatalf("expected no contact, got %+v", data.Contact)
	}
	if len(data.Recipients) != 1 {
		t.Fatalf("expected 1 recipient record, got %d", len(data.Recipients))
	}
}

func TestDeleteSubjectDataRemovesContactAndRecipientsNotMessage(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)

	c, err := db.CreateContact(ctx, NewContact{TenantID: tn.ID, Email: "erase@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	msg := NewMessage{
		ID: id, TenantID: tn.ID, MailFrom: "<sender@example.com>",
		FromHeader: "Sender <sender@example.com>", Subject: "hi",
		MessageIDHeader: "<" + id + "@example.com>",
		Recipients: []RecipientInput{
			{Address: "erase@example.com", HeaderKind: strPtr("to")},
			{Address: "other@example.com", HeaderKind: strPtr("to")},
		},
	}
	if _, err := db.InsertMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}

	result, err := db.DeleteSubjectData(ctx, tn.ID, "erase@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !result.ContactDeleted || result.RecipientsDeleted != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}

	if _, err := db.GetContact(ctx, tn.ID, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected contact gone, got %v", err)
	}
	// The message itself and the OTHER recipient must survive - erasure
	// scope is the subject's own rows, not the whole message.
	if _, err := db.GetMessage(ctx, tn.ID, id); err != nil {
		t.Fatalf("expected message to survive: %v", err)
	}
	var otherCount int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM recipients WHERE message_id = $1 AND address = 'other@example.com'`, id).Scan(&otherCount); err != nil {
		t.Fatal(err)
	}
	if otherCount != 1 {
		t.Fatalf("expected the other recipient to survive, got count=%d", otherCount)
	}
}

// TestDeleteSubjectDataRemovesMatchingBroadcastRecipientSnapshot proves the
// Greptile-flagged bug fix: broadcast_recipients keeps its OWN snapshot of
// a contact (email/name/attributes at broadcast-creation time), not
// reachable via the contacts FK - so erasure must independently find and
// remove matching broadcast_recipients rows, not just the contacts/
// recipients rows.
func TestDeleteSubjectDataRemovesMatchingBroadcastRecipientSnapshot(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)

	aud, err := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "gdpr-test"})
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := db.CreateTemplate(ctx, NewTemplate{TenantID: tn.ID, Name: "gdpr-tmpl", Subject: "hi", HTML: "<p>hi</p>"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateBroadcast(ctx, NewBroadcast{TenantID: tn.ID, Name: "gdpr-broadcast", AudienceID: aud.ID, TemplateID: tmpl.ID, FromAddress: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}

	matchID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately NO contact_id / no matching contacts row - proves this
	// snapshot is found and removed independently of the contacts FK path.
	dummyContactID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx,
		`INSERT INTO broadcast_recipients (id, broadcast_id, tenant_id, contact_id, email, status) VALUES ($1,$2,$3,$4,$5,'pending')`,
		matchID, b.ID, tn.ID, dummyContactID, "erase@example.com"); err != nil {
		t.Fatal(err)
	}
	otherID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	dummyContactID2, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx,
		`INSERT INTO broadcast_recipients (id, broadcast_id, tenant_id, contact_id, email, status) VALUES ($1,$2,$3,$4,$5,'pending')`,
		otherID, b.ID, tn.ID, dummyContactID2, "keep@example.com"); err != nil {
		t.Fatal(err)
	}

	result, err := db.DeleteSubjectData(ctx, tn.ID, "erase@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if result.BroadcastRecipientsDeleted != 1 {
		t.Fatalf("expected 1 broadcast_recipients row deleted, got %+v", result)
	}
	var count int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_recipients WHERE id = $1`, matchID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("expected the matching broadcast_recipients snapshot to be deleted")
	}
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_recipients WHERE id = $1`, otherID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("expected the OTHER broadcast_recipients row to survive")
	}
}

func TestDeleteSubjectDataNoMatchIsNotAnError(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)

	result, err := db.DeleteSubjectData(ctx, tn.ID, "nobody@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if result.ContactDeleted || result.RecipientsDeleted != 0 {
		t.Fatalf("expected a no-op result, got %+v", result)
	}
}

func TestExportSubjectDataIncludesBroadcastSnapshots(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)

	aud, err := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "gdpr-export-test"})
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := db.CreateTemplate(ctx, NewTemplate{TenantID: tn.ID, Name: "gdpr-export-tmpl", Subject: "hi", HTML: "<p>hi</p>"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateBroadcast(ctx, NewBroadcast{TenantID: tn.ID, Name: "gdpr-export-broadcast", AudienceID: aud.ID, TemplateID: tmpl.ID, FromAddress: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	snapID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	dummyContactID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx,
		`INSERT INTO broadcast_recipients (id, broadcast_id, tenant_id, contact_id, email, name, attributes, status) VALUES ($1,$2,$3,$4,$5,$6,$7,'materialized')`,
		snapID, b.ID, tn.ID, dummyContactID, "snapshot@example.com", "Snapshot Person", []byte(`{"plan":"pro"}`)); err != nil {
		t.Fatal(err)
	}

	data, err := db.ExportSubjectData(ctx, tn.ID, "snapshot@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(data.BroadcastSnapshots) != 1 {
		t.Fatalf("expected 1 broadcast snapshot in export, got %d", len(data.BroadcastSnapshots))
	}
	s := data.BroadcastSnapshots[0]
	if s.ID != snapID || s.Name != "Snapshot Person" || s.Attributes["plan"] != "pro" {
		t.Fatalf("unexpected snapshot data: %+v", s)
	}
}
