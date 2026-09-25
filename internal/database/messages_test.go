package database

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func strPtr(s string) *string { return &s }

func sampleNewMessage(t *testing.T, tenantID string) NewMessage {
	t.Helper()
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	return NewMessage{
		ID:              id,
		TenantID:        tenantID,
		MailFrom:        "<alice@example.com>",
		FromHeader:      "Alice <alice@example.com>",
		Subject:         "hello",
		MessageIDHeader: "<abc@example.com>",
		Recipients: []RecipientInput{
			{Address: "<bob@example.com>", HeaderKind: strPtr("to")},
			{Address: "<hidden-bcc@example.com>"}, // envelope-only, no header
		},
	}
}

func TestInsertMessageCreatesMessageRecipientsAndEvent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	in := sampleNewMessage(t, tenant.ID)
	msg, err := db.InsertMessage(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Status != StatusQueued || msg.QueuedAt == nil {
		t.Fatalf("expected queued status with QueuedAt set, got %+v", msg)
	}

	recipients, err := db.ListRecipients(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recipients) != 2 {
		t.Fatalf("expected 2 recipients, got %d", len(recipients))
	}
	var sawHiddenBcc bool
	for _, r := range recipients {
		if r.Address == "<hidden-bcc@example.com>" {
			sawHiddenBcc = true
			if r.HeaderKind != nil {
				t.Fatalf("hidden bcc must have nil header_kind, got %v", *r.HeaderKind)
			}
		}
		if r.Status != RecipientPending {
			t.Fatalf("new recipient must be pending, got %v", r.Status)
		}
	}
	if !sawHiddenBcc {
		t.Fatal("hidden bcc recipient not persisted")
	}

	events, err := db.ListMessageEvents(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != EventQueued {
		t.Fatalf("expected 1 queued event, got %+v", events)
	}
}

func TestInsertMessageRejectsInvalidInput(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	cases := []struct {
		name string
		mod  func(*NewMessage)
	}{
		{"empty id", func(n *NewMessage) { n.ID = "" }},
		{"empty tenant", func(n *NewMessage) { n.TenantID = "" }},
		{"empty mail_from", func(n *NewMessage) { n.MailFrom = "" }},
		{"no recipients", func(n *NewMessage) { n.Recipients = nil }},
		{"empty recipient address", func(n *NewMessage) { n.Recipients[0].Address = "" }},
		{"bad header_kind", func(n *NewMessage) { n.Recipients[0].HeaderKind = strPtr("bogus") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := sampleNewMessage(t, tenant.ID)
			tc.mod(&in)
			if _, err := db.InsertMessage(ctx, in); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestInsertMessageDuplicateIDIsConflict(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	in := sampleNewMessage(t, tenant.ID)
	if _, err := db.InsertMessage(ctx, in); err != nil {
		t.Fatal(err)
	}
	// Same ID again (fresh recipients slice, doesn't matter).
	if _, err := db.InsertMessage(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict for duplicate message ID, got %v", err)
	}
}

func TestInsertMessageInvalidTenantIsConflict(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	in := sampleNewMessage(t, "does-not-exist")
	if _, err := db.InsertMessage(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict for invalid tenant FK, got %v", err)
	}
}

func TestInsertMessagePartialFailureRollsBack(t *testing.T) {
	// A message with a header_kind so invalid it fails the CHECK
	// constraint at INSERT time (bypassing Go validation by direct SQL)
	// proves the transaction leaves no partial message/recipient rows.
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	// Two recipients with the SAME address AND role violate the v0.18
	// UNIQUE(message_id, address, header_kind) constraint on the second
	// insert, mid-batch — the message row itself must not survive since
	// it was in the same transaction. (Same address in different roles,
	// or both with a NULL/unclassified role, is deliberately now allowed
	// — see migration 000003 — so this test pins the role to make the
	// two rows genuinely duplicate.)
	to := "to"
	in := sampleNewMessage(t, tenant.ID)
	in.Recipients = []RecipientInput{
		{Address: "<dup@example.com>", HeaderKind: &to},
		{Address: "<dup@example.com>", HeaderKind: &to},
	}
	if _, err := db.InsertMessage(ctx, in); err == nil {
		t.Fatal("expected duplicate-recipient constraint violation")
	}
	if _, err := db.GetMessage(ctx, tenant.ID, in.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("message must not exist after rolled-back transaction, got %v", err)
	}
}

func TestInsertMessageAllowsSameAddressInMultipleRoles(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	to, cc := "to", "cc"
	in := sampleNewMessage(t, tenant.ID)
	in.Recipients = []RecipientInput{
		{Address: "<both@example.com>", HeaderKind: &to},
		{Address: "<both@example.com>", HeaderKind: &cc},
	}
	msg, err := db.InsertMessage(ctx, in)
	if err != nil {
		t.Fatalf("same address in different roles must be allowed: %v", err)
	}
	recipients, err := db.ListRecipients(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recipients) != 2 {
		t.Fatalf("expected 2 distinct role rows, got %d", len(recipients))
	}
}

func TestGetMessageTenantScoped(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenantA := newTestTenant(t, db)
	tenantB, err := db.CreateTenant(ctx, "tenant-b")
	if err != nil {
		t.Fatal(err)
	}

	in := sampleNewMessage(t, tenantA.ID)
	msg, err := db.InsertMessage(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.GetMessage(ctx, tenantA.ID, msg.ID); err != nil {
		t.Fatalf("owning tenant must find the message: %v", err)
	}
	if _, err := db.GetMessage(ctx, tenantB.ID, msg.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant lookup must return ErrNotFound, not leak the row: %v", err)
	}
}

func TestListMessagesTenantIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenantA := newTestTenant(t, db)
	tenantB, err := db.CreateTenant(ctx, "tenant-b")
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if _, err := db.InsertMessage(ctx, sampleNewMessage(t, tenantA.ID)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := db.InsertMessage(ctx, sampleNewMessage(t, tenantB.ID)); err != nil {
			t.Fatal(err)
		}
	}

	listA, err := db.ListMessages(ctx, tenantA.ID, nil, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listA) != 3 {
		t.Fatalf("expected 3 messages for tenant A, got %d", len(listA))
	}
	for _, m := range listA {
		if m.TenantID != tenantA.ID {
			t.Fatalf("tenant isolation violated: got message from tenant %q while listing tenant %q", m.TenantID, tenantA.ID)
		}
	}

	listB, err := db.ListMessages(ctx, tenantB.ID, nil, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listB) != 2 {
		t.Fatalf("expected 2 messages for tenant B, got %d", len(listB))
	}
}

func TestListMessagesOrderingAndCursorPagination(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	const n = 5
	var ids []string
	for i := 0; i < n; i++ {
		msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, msg.ID)
	}

	// Page through with limit=2, expecting newest-first ordering split
	// across pages via keyset pagination — no duplicates, no gaps.
	var seen []string
	var cursor *MessageCursor
	for {
		page, err := db.ListMessages(ctx, tenant.ID, nil, 2, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, m := range page {
			seen = append(seen, m.ID)
		}
		last := page[len(page)-1]
		cursor = &MessageCursor{CreatedAt: last.CreatedAt, ID: last.ID}
		if len(page) < 2 {
			break
		}
	}
	if len(seen) != n {
		t.Fatalf("pagination lost or duplicated rows: got %d, want %d (%v)", len(seen), n, seen)
	}
	seenSet := map[string]bool{}
	for _, id := range seen {
		if seenSet[id] {
			t.Fatalf("duplicate row across pages: %s", id)
		}
		seenSet[id] = true
	}
	for _, id := range ids {
		if !seenSet[id] {
			t.Fatalf("message %s missing from paginated results", id)
		}
	}
}

func TestListMessagesFilterByStatus(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	msg1, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID)); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMessageStatus(ctx, msg1.ID, StatusDelivered, timePtr(time.Now())); err != nil {
		t.Fatal(err)
	}

	delivered := StatusDelivered
	list, err := db.ListMessages(ctx, tenant.ID, &delivered, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != msg1.ID {
		t.Fatalf("expected exactly the delivered message, got %+v", list)
	}
}

func TestListMessagesRejectsInvalidLimit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	if _, err := db.ListMessages(ctx, tenant.ID, nil, 0, nil); err == nil {
		t.Fatal("expected error for limit=0")
	}
	if _, err := db.ListMessages(ctx, tenant.ID, nil, 1000, nil); err == nil {
		t.Fatal("expected error for limit>500")
	}
}

func timePtr(t time.Time) *time.Time { return &t }

func TestUpdateMessageStatusTransitionsAndDeliveredAt(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	if msg.DeliveredAt != nil {
		t.Fatal("new message must not have DeliveredAt set")
	}

	// Postgres timestamptz only stores microsecond precision, so any
	// finer-grained wall-clock reading gets truncated on round trip. This
	// is invisible on a clock whose native resolution is already >= 1µs
	// (true of this test suite's usual macOS dev environment) but real
	// on Linux, where time.Now() carries genuine nanosecond entropy -
	// truncate up front so the comparison reflects what is actually
	// stored, not the clock's raw resolution.
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := db.UpdateMessageStatus(ctx, msg.ID, StatusDelivered, &now); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetMessage(ctx, tenant.ID, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDelivered {
		t.Fatalf("status not updated: %v", got.Status)
	}
	if got.DeliveredAt == nil || !got.DeliveredAt.Equal(now) {
		t.Fatalf("DeliveredAt not set correctly: %v", got.DeliveredAt)
	}
	if !got.UpdatedAt.After(msg.UpdatedAt) && !got.UpdatedAt.Equal(msg.UpdatedAt) {
		t.Fatalf("UpdatedAt must advance: before=%v after=%v", msg.UpdatedAt, got.UpdatedAt)
	}
}

func TestUpdateMessageStatusUnknownIDIsNotFound(t *testing.T) {
	db := newTestDB(t)
	if err := db.UpdateMessageStatus(context.Background(), "does-not-exist", StatusFailed, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestUpdateMessageStatusRejectsInvalidStatus(t *testing.T) {
	db := newTestDB(t)
	if err := db.UpdateMessageStatus(context.Background(), "whatever", MessageStatus("bogus"), nil); err == nil {
		t.Fatal("expected error for invalid status")
	}
}

// -------------------------------------------------------- concurrency ---

func TestConcurrentMessageInserts(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent insert failed: %v", err)
		}
	}
	list, err := db.ListMessages(ctx, tenant.ID, nil, 500, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != n {
		t.Fatalf("expected %d messages, got %d", n, len(list))
	}
}

func TestConcurrentRecipientStatusUpdatesSameMessage(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	in := sampleNewMessage(t, tenant.ID)
	in.Recipients = []RecipientInput{{Address: "<a@example.com>"}}
	msg, err := db.InsertMessage(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status := RecipientDelivered
			if i%2 == 0 {
				status = RecipientFailed
			}
			err := db.UpdateRecipientStatuses(ctx, msg.ID, []RecipientStatusUpdate{
				{Address: "<a@example.com>", Status: status, SMTPCode: 250},
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent recipient update failed: %v", err)
		}
	}
	// Whatever the last write was, exactly one row must exist with a valid status.
	recipients, err := db.ListRecipients(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recipients) != 1 {
		t.Fatalf("expected 1 recipient row (no duplication from concurrent updates), got %d", len(recipients))
	}
}

func TestUpdateRecipientStatusesUnknownRecipientIsNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	err = db.UpdateRecipientStatuses(ctx, msg.ID, []RecipientStatusUpdate{
		{Address: "<does-not-exist@example.com>", Status: RecipientFailed},
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestContextCancellationDuringQuery(t *testing.T) {
	db := newTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tenant := newTestTenant(t, db) // uses its own ctx internally via newTestDB helper's background
	_, err := db.ListMessages(ctx, tenant.ID, nil, 10, nil)
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
}

var _ = fmt.Sprintf

// TestInsertMessageRequireBroadcastRecipientIDBlocksOnAlreadyDeletedRow
// proves the fix for the race Greptile flagged: a broadcast_recipients row
// erased (e.g. by GDPR gdpr-delete) between being claimed by the expander
// and this insert must stop the insert, atomically (via FOR UPDATE row
// locking), rather than trusting stale in-memory claim data.
func TestInsertMessageRequireBroadcastRecipientIDBlocksOnAlreadyDeletedRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)

	aud, err := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "race-test"})
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := db.CreateTemplate(ctx, NewTemplate{TenantID: tn.ID, Name: "race-tmpl", Subject: "hi", HTML: "<p>hi</p>"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateBroadcast(ctx, NewBroadcast{TenantID: tn.ID, Name: "race-broadcast", AudienceID: aud.ID, TemplateID: tmpl.ID, FromAddress: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	recID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	dummyContactID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx,
		`INSERT INTO broadcast_recipients (id, broadcast_id, tenant_id, contact_id, email, status) VALUES ($1,$2,$3,$4,$5,'pending')`,
		recID, b.ID, tn.ID, dummyContactID, "raced@example.com"); err != nil {
		t.Fatal(err)
	}

	// Simulate the erasure: delete the row exactly as gdpr-delete does,
	// BEFORE the "expander" (this test) gets to InsertMessage.
	if _, err := db.pool.Exec(ctx, `DELETE FROM broadcast_recipients WHERE id = $1`, recID); err != nil {
		t.Fatal(err)
	}

	msg := sampleNewMessage(t, tn.ID)
	msg.RequireBroadcastRecipientID = recID
	if _, err := db.InsertMessage(ctx, msg); !errors.Is(err, ErrRecipientErased) {
		t.Fatalf("expected ErrRecipientErased, got %v", err)
	}
	if _, err := db.GetMessage(ctx, tn.ID, msg.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected no message to have been inserted, got %v", err)
	}
}

// TestInsertMessageRequireBroadcastRecipientIDAllowsExistingRow is the
// control case: the row still exists, so the insert must proceed exactly
// as it would without RequireBroadcastRecipientID set.
func TestInsertMessageRequireBroadcastRecipientIDAllowsExistingRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)

	aud, err := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "race-test-ok"})
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := db.CreateTemplate(ctx, NewTemplate{TenantID: tn.ID, Name: "race-tmpl-ok", Subject: "hi", HTML: "<p>hi</p>"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateBroadcast(ctx, NewBroadcast{TenantID: tn.ID, Name: "race-broadcast-ok", AudienceID: aud.ID, TemplateID: tmpl.ID, FromAddress: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	recID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	dummyContactID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx,
		`INSERT INTO broadcast_recipients (id, broadcast_id, tenant_id, contact_id, email, status) VALUES ($1,$2,$3,$4,$5,'pending')`,
		recID, b.ID, tn.ID, dummyContactID, "notraced@example.com"); err != nil {
		t.Fatal(err)
	}

	msg := sampleNewMessage(t, tn.ID)
	msg.RequireBroadcastRecipientID = recID
	if _, err := db.InsertMessage(ctx, msg); err != nil {
		t.Fatalf("expected insert to succeed while the recipient row still exists: %v", err)
	}
}
