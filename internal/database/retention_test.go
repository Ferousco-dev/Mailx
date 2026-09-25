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

// markMessageTerminal sets a message's status directly to a terminal value
// (InsertMessage always starts a message at 'queued', which
// PurgeExpiredMessages must never purge - see its non-terminal-status
// exclusion).
func markMessageTerminal(t *testing.T, db *DB, messageID, status string) {
	t.Helper()
	if _, err := db.pool.Exec(context.Background(), `UPDATE messages SET status = $1 WHERE id = $2`, status, messageID); err != nil {
		t.Fatal(err)
	}
}

// noopDeleteDisk is the deleteDisk callback for tests that don't exercise
// on-disk cleanup themselves - every candidate id "succeeds".
func noopDeleteDisk(string) error { return nil }

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
	markMessageTerminal(t, db, strictMsg.ID, "delivered")
	markMessageTerminal(t, db, lenientMsg.ID, "delivered")

	purged, err := db.PurgeExpiredMessages(ctx, noopDeleteDisk)
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
	markMessageTerminal(t, db, msg.ID, "delivered")

	if _, err := db.PurgeExpiredMessages(ctx, noopDeleteDisk); err != nil {
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
	markMessageTerminal(t, db, msg.ID, "delivered")

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

	if _, err := db.PurgeExpiredMessages(ctx, noopDeleteDisk); err != nil {
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

// TestPurgeExpiredMessagesExcludesNonTerminalStatus proves the Greptile-
// flagged bug fix: a message still 'queued' (e.g. accepted far in advance
// for a scheduled send, or simply not yet picked up) must never be purged
// just because its created_at is old - only a terminal status makes it
// eligible.
func TestPurgeExpiredMessagesExcludesNonTerminalStatus(t *testing.T) {
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
	// Old enough to be past the 1-day window, but still 'queued' (a
	// scheduled send far in the future, or simply pending) - InsertMessage
	// already leaves it at 'queued'.
	backdateMessage(t, db, msg.ID, time.Now().UTC().Add(-30*24*time.Hour))

	purged, err := db.PurgeExpiredMessages(ctx, noopDeleteDisk)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 0 {
		t.Fatalf("expected a queued message to survive purge, got purged=%v", purged)
	}
	if _, err := db.GetMessage(ctx, tn.ID, msg.ID); err != nil {
		t.Fatalf("expected queued message to still exist: %v", err)
	}
}

// TestPurgeExpiredMessagesKeepsDBRowWhenDiskDeleteFails proves the other
// Greptile-flagged bug fix: if the disk-cleanup callback fails for an id,
// that id's database row must NOT be deleted - it must survive to be
// retried (disk and DB together) on a later purge, never leaving an
// orphaned on-disk file with no DB row pointing at it.
func TestPurgeExpiredMessagesKeepsDBRowWhenDiskDeleteFails(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)

	one := 1
	if err := db.SetTenantRetention(ctx, tn.ID, &one); err != nil {
		t.Fatal(err)
	}
	failMsg, err := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	if err != nil {
		t.Fatal(err)
	}
	okMsg, err := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	backdateMessage(t, db, failMsg.ID, old)
	backdateMessage(t, db, okMsg.ID, old)
	markMessageTerminal(t, db, failMsg.ID, "delivered")
	markMessageTerminal(t, db, okMsg.ID, "delivered")

	deleteDisk := func(id string) error {
		if id == failMsg.ID {
			return errors.New("simulated disk failure")
		}
		return nil
	}
	purged, err := db.PurgeExpiredMessages(ctx, deleteDisk)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 1 || purged[0] != okMsg.ID {
		t.Fatalf("expected only %s purged, got %v", okMsg.ID, purged)
	}
	if _, err := db.GetMessage(ctx, tn.ID, failMsg.ID); err != nil {
		t.Fatalf("expected the disk-delete-failed message's DB row to survive: %v", err)
	}
	if _, err := db.GetMessage(ctx, tn.ID, okMsg.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected the successfully disk-deleted message's DB row to be gone: %v", err)
	}
}

// TestPurgeExpiredMessagesSkipsPastConsecutiveFailures proves the fix for
// the starvation Greptile flagged: several persistently-failing OLDEST
// candidates must not prevent a NEWER, successfully-deleted candidate from
// being purged in the same call. (Reproducing the original bug at its real
// scale - over purgeBatchLimit=1000 failing candidates persisting across
// multiple purge ticks - is impractical for a unit test; this exercises
// the same underlying mechanism, collectPurgeableIDs's cursor advancing
// past a failure instead of getting stuck re-selecting it, at a small
// scale.)
func TestPurgeExpiredMessagesSkipsPastConsecutiveFailures(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)

	one := 1
	if err := db.SetTenantRetention(ctx, tn.ID, &one); err != nil {
		t.Fatal(err)
	}

	const numFailing = 5
	failingIDs := make(map[string]bool, numFailing)
	old := time.Now().UTC().Add(-48 * time.Hour)
	for i := 0; i < numFailing; i++ {
		msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
		if err != nil {
			t.Fatal(err)
		}
		backdateMessage(t, db, msg.ID, old.Add(time.Duration(i)*time.Second)) // oldest first
		markMessageTerminal(t, db, msg.ID, "delivered")
		failingIDs[msg.ID] = true
	}
	okMsg, err := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	if err != nil {
		t.Fatal(err)
	}
	backdateMessage(t, db, okMsg.ID, old.Add(numFailing*time.Second)) // newest of the expired set
	markMessageTerminal(t, db, okMsg.ID, "delivered")

	deleteDisk := func(id string) error {
		if failingIDs[id] {
			return errors.New("simulated persistent disk failure")
		}
		return nil
	}
	purged, err := db.PurgeExpiredMessages(ctx, deleteDisk)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 1 || purged[0] != okMsg.ID {
		t.Fatalf("expected the newer message to be purged despite older persistent failures ahead of it, got %v", purged)
	}
	for id := range failingIDs {
		if _, err := db.GetMessage(ctx, tn.ID, id); err != nil {
			t.Fatalf("expected failing message %s's row to survive for retry: %v", id, err)
		}
	}
}
