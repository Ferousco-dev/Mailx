package database

import (
	"context"
	"fmt"
	"testing"
)

// setupAudienceFixture creates the tenant/audience/template but NOT the
// broadcast — callers add audience members first, then call createBroadcast,
// so the broadcast's audience_snapshot_at watermark is naturally AFTER those
// members (matching real "audience populated, then a broadcast accepted"
// ordering, not the other way around).
func setupAudienceFixture(t *testing.T, db *DB) (tn Tenant, aud Audience, tmpl Template) {
	t.Helper()
	ctx := context.Background()
	tn = newTestTenant(t, db)
	aud, err := db.CreateAudience(ctx, NewAudience{TenantID: tn.ID, Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err = db.CreateTemplate(ctx, NewTemplate{TenantID: tn.ID, Name: "t", Subject: "s", Text: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return
}

func createBroadcast(t *testing.T, db *DB, tn Tenant, aud Audience, tmpl Template) Broadcast {
	t.Helper()
	b, err := db.CreateBroadcast(context.Background(), sampleBroadcast(tn.ID, aud.ID, tmpl.ID))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// setupBroadcastFixture is for tests that don't care about member ordering
// relative to the watermark (e.g. claim/complete/guard tests).
func setupBroadcastFixture(t *testing.T, db *DB) (tn Tenant, aud Audience, b Broadcast) {
	t.Helper()
	var tmpl Template
	tn, aud, tmpl = setupAudienceFixture(t, db)
	b = createBroadcast(t, db, tn, aud, tmpl)
	return
}

func TestSnapshotBroadcastBatchBoundedAndResumable(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn, aud, tmpl := setupAudienceFixture(t, db)
	for i := 0; i < 25; i++ {
		c := mustContact(t, db, tn.ID, fmt.Sprintf("c%d@example.com", i))
		if err := db.AddAudienceMember(ctx, tn.ID, aud.ID, c.ID); err != nil {
			t.Fatal(err)
		}
	}
	b := createBroadcast(t, db, tn, aud, tmpl)
	total := 0
	for i := 0; i < 100; i++ { // bounded loop guard, real loop ends on exhausted
		advanced, exhausted, err := db.SnapshotBroadcastBatch(ctx, b, 10)
		if err != nil {
			t.Fatal(err)
		}
		if advanced > 10 {
			t.Fatalf("batch returned %d rows, want <= 10 (unbounded)", advanced)
		}
		total += advanced
		b, err = db.GetBroadcast(ctx, tn.ID, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if exhausted {
			break
		}
	}
	if total != 25 {
		t.Fatalf("snapshotted %d, want 25", total)
	}
	if !b.SnapshotComplete {
		t.Fatal("snapshot never completed")
	}
	var n int
	db.pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_recipients WHERE broadcast_id = $1`, b.ID).Scan(&n)
	if n != 25 {
		t.Fatalf("%d recipient rows, want 25", n)
	}
}

// A member added AFTER the broadcast's audience_snapshot_at watermark must
// never be included, however many snapshot batches run afterward.
func TestSnapshotBroadcastBatchExcludesMembersAddedAfterWatermark(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn, aud, tmpl := setupAudienceFixture(t, db)
	early := mustContact(t, db, tn.ID, "early@example.com")
	if err := db.AddAudienceMember(ctx, tn.ID, aud.ID, early.ID); err != nil {
		t.Fatal(err)
	}
	// The broadcast's watermark is taken HERE, before "late" is added.
	b := createBroadcast(t, db, tn, aud, tmpl)
	late := mustContact(t, db, tn.ID, "late@example.com")
	if err := db.AddAudienceMember(ctx, tn.ID, aud.ID, late.ID); err != nil {
		t.Fatal(err)
	}
	_, exhausted, err := db.SnapshotBroadcastBatch(ctx, b, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !exhausted {
		t.Fatal("expected exhaustion in one batch")
	}
	var n int
	db.pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_recipients WHERE broadcast_id = $1 AND contact_id = $2`, b.ID, late.ID).Scan(&n)
	if n != 0 {
		t.Fatal("member added after the watermark was included")
	}
	db.pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_recipients WHERE broadcast_id = $1`, b.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("%d rows, want exactly 1 (early only)", n)
	}
}

// Re-running the SAME batch range after a "crash" (never advancing past it)
// must not create duplicate recipient rows.
func TestSnapshotBroadcastBatchRetryIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn, aud, tmpl := setupAudienceFixture(t, db)
	for i := 0; i < 5; i++ {
		c := mustContact(t, db, tn.ID, fmt.Sprintf("c%d@example.com", i))
		db.AddAudienceMember(ctx, tn.ID, aud.ID, c.ID)
	}
	b := createBroadcast(t, db, tn, aud, tmpl)
	// First run: advances the cursor and inserts 5 rows.
	if _, _, err := db.SnapshotBroadcastBatch(ctx, b, 10); err != nil {
		t.Fatal(err)
	}
	// "Crash before the cursor update was durable" simulated by re-running
	// the SAME (stale, pre-advance) broadcast value again.
	if _, _, err := db.SnapshotBroadcastBatch(ctx, b, 10); err != nil {
		t.Fatal(err)
	}
	var n int
	db.pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_recipients WHERE broadcast_id = $1`, b.ID).Scan(&n)
	if n != 5 {
		t.Fatalf("%d rows after retried batch, want 5 (no duplicates)", n)
	}
}

func TestClaimActiveBroadcastsSkipsCompletedAndConcurrentClaims(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	_, _, b := setupBroadcastFixture(t, db)

	tx1, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback(ctx)
	var id string
	if err := tx1.QueryRow(ctx, `SELECT id FROM broadcasts WHERE id = $1 FOR UPDATE SKIP LOCKED`, b.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}

	// A second (simulated concurrent) claim must skip the locked row.
	claimed, err := db.ClaimActiveBroadcasts(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range claimed {
		if c.ID == b.ID {
			t.Fatal("a locked broadcast was claimed concurrently")
		}
	}
}

func TestPendingAndMarkRecipientGuards(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn, aud, tmpl := setupAudienceFixture(t, db)
	c := mustContact(t, db, tn.ID, "one@example.com")
	db.AddAudienceMember(ctx, tn.ID, aud.ID, c.ID)
	b := createBroadcast(t, db, tn, aud, tmpl)
	db.SnapshotBroadcastBatch(ctx, b, 10)

	pending, err := db.PendingBroadcastRecipients(ctx, b.ID, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("%v %v", pending, err)
	}
	r := pending[0]
	if err := db.MarkBroadcastRecipientMaterialized(ctx, r.ID, "msg-1"); err != nil {
		t.Fatal(err)
	}
	// Guarded: a second, different transition must be a no-op, not an error
	// and not overwrite the terminal state.
	if err := db.MarkBroadcastRecipientSuppressed(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	pending, _ = db.PendingBroadcastRecipients(ctx, b.ID, 10)
	if len(pending) != 0 {
		t.Fatalf("%d still pending", len(pending))
	}
	var status string
	db.pool.QueryRow(ctx, `SELECT status FROM broadcast_recipients WHERE id = $1`, r.ID).Scan(&status)
	if status != "materialized" {
		t.Fatalf("status = %q, want materialized (the later suppressed call must not overwrite it)", status)
	}
}

func TestTryCompleteBroadcast(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn, aud, tmpl := setupAudienceFixture(t, db)
	c := mustContact(t, db, tn.ID, "one@example.com")
	db.AddAudienceMember(ctx, tn.ID, aud.ID, c.ID)
	b := createBroadcast(t, db, tn, aud, tmpl)
	db.MarkBroadcastExpanding(ctx, b.ID)
	db.SnapshotBroadcastBatch(ctx, b, 10)

	completed, err := db.TryCompleteBroadcast(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed {
		t.Fatal("must not complete while a recipient is still pending")
	}
	pending, _ := db.PendingBroadcastRecipients(ctx, b.ID, 10)
	db.MarkBroadcastRecipientMaterialized(ctx, pending[0].ID, "msg-1")

	completed, err = db.TryCompleteBroadcast(ctx, b.ID)
	if err != nil || !completed {
		t.Fatalf("completed=%v err=%v", completed, err)
	}
	got, _ := db.GetBroadcast(ctx, tn.ID, b.ID)
	if got.Status != "completed" {
		t.Fatalf("status = %q", got.Status)
	}
}

// Empty audience: snapshot exhausts immediately with zero rows, and the
// broadcast completes coherently with zero recipients.
func TestEmptyAudienceCompletesCleanly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	_, _, b := setupBroadcastFixture(t, db)
	db.MarkBroadcastExpanding(ctx, b.ID)
	_, exhausted, err := db.SnapshotBroadcastBatch(ctx, b, 10)
	if err != nil || !exhausted {
		t.Fatalf("%v %v", exhausted, err)
	}
	completed, err := db.TryCompleteBroadcast(ctx, b.ID)
	if err != nil || !completed {
		t.Fatalf("completed=%v err=%v", completed, err)
	}
}

func TestMarkBroadcastFailedStopsClaiming(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	_, _, b := setupBroadcastFixture(t, db)
	if err := db.MarkBroadcastFailed(ctx, b.ID, "from_domain_not_authorized"); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimActiveBroadcasts(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range claimed {
		if c.ID == b.ID {
			t.Fatal("a failed broadcast was claimed for further work")
		}
	}
	var status, reason string
	db.pool.QueryRow(ctx, `SELECT status, failure_reason FROM broadcasts WHERE id = $1`, b.ID).Scan(&status, &reason)
	if status != "failed" || reason != "from_domain_not_authorized" {
		t.Fatalf("%q %q", status, reason)
	}
}
