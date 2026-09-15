package database

import (
	"context"
	"testing"
)

// TestParameterizedQueriesRejectSQLInjectionAttempts proves hostile string
// values (tenant name, subject, recipient address) are always treated as
// DATA, never as SQL, because every query in this package uses parameter
// placeholders ($1, $2, ...) — never string concatenation/fmt.Sprintf into
// SQL text.
func TestParameterizedQueriesRejectSQLInjectionAttempts(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	hostileName := `'; DROP TABLE tenants; --`
	tenant, err := db.CreateTenant(ctx, hostileName)
	if err != nil {
		t.Fatal(err)
	}
	if tenant.Name != hostileName {
		t.Fatalf("hostile name was not stored verbatim as data: %q", tenant.Name)
	}

	// The tenants table must still exist and be queryable — proves no
	// injected DROP TABLE executed.
	if _, err := db.GetTenant(ctx, tenant.ID); err != nil {
		t.Fatalf("tenants table damaged by injection attempt: %v", err)
	}

	in := sampleNewMessage(t, tenant.ID)
	in.Subject = `'); DELETE FROM messages; --`
	in.Recipients[0].Address = `<a@x>'); DROP TABLE recipients; --`
	msg, err := db.InsertMessage(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Subject != in.Subject {
		t.Fatalf("hostile subject not stored verbatim: %q", msg.Subject)
	}

	list, err := db.ListMessages(ctx, tenant.ID, nil, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("messages table damaged by injection attempt: %d rows", len(list))
	}
}

func TestHugeButBoundedRecipientCount(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	in := sampleNewMessage(t, tenant.ID)
	in.Recipients = nil
	// 500 recipients — well within realistic RFC 5321 §4.5.3.1.8 minimums
	// (100) and MailX's own retry.DefaultAttemptLimit-adjacent scale;
	// large enough to prove the batched recipient insert handles a
	// non-trivial count without per-row round trips failing partway.
	for i := 0; i < 500; i++ {
		in.Recipients = append(in.Recipients, RecipientInput{Address: sprintfAddr(i)})
	}
	msg, err := db.InsertMessage(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	recipients, err := db.ListRecipients(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recipients) != 500 {
		t.Fatalf("expected 500 recipients, got %d", len(recipients))
	}
}

func sprintfAddr(i int) string {
	const hex = "0123456789abcdef"
	b := []byte("<r0000@example.com>")
	b[2] = hex[(i>>12)&0xf]
	b[3] = hex[(i>>8)&0xf]
	b[4] = hex[(i>>4)&0xf]
	b[5] = hex[i&0xf]
	return string(b)
}

func TestInvalidForeignKeyOnDeliveryAttempt(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	_, err := db.InsertDeliveryAttempt(ctx, sampleAttempt("does-not-exist", 1, DecisionRetry))
	if err == nil {
		t.Fatal("expected FK violation for nonexistent message_id")
	}
}
