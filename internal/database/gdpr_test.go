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
