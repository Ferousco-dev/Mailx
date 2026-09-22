package database

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
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

	pending, err := db.ClaimPendingBroadcastRecipients(ctx, b.ID, 10)
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
	pending, _ = db.ClaimPendingBroadcastRecipients(ctx, b.ID, 10)
	if len(pending) != 0 {
		t.Fatalf("%d still pending", len(pending))
	}
	var status string
	db.pool.QueryRow(ctx, `SELECT status FROM broadcast_recipients WHERE id = $1`, r.ID).Scan(&status)
	if status != "materialized" {
		t.Fatalf("status = %q, want materialized (the later suppressed call must not overwrite it)", status)
	}
}

// TestClaimPendingBroadcastRecipientsStaysClaimedAcrossGap proves the PR
// review fix: unlike a plain SELECT ... FOR UPDATE SKIP LOCKED (whose lock
// disappears the instant the query returns), a claimed row must stay
// unavailable to a second claimant for the lease duration even AFTER the
// claiming statement has committed and returned — covering the real window
// (render/sign/InsertMessage) during which the original claimant is still
// working it.
func TestClaimPendingBroadcastRecipientsStaysClaimedAcrossGap(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn, aud, tmpl := setupAudienceFixture(t, db)
	c := mustContact(t, db, tn.ID, "one@example.com")
	db.AddAudienceMember(ctx, tn.ID, aud.ID, c.ID)
	b := createBroadcast(t, db, tn, aud, tmpl)
	db.SnapshotBroadcastBatch(ctx, b, 10)

	first, err := db.ClaimPendingBroadcastRecipients(ctx, b.ID, 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("%v %v", first, err)
	}
	// The claiming statement has already committed (Query returned) — a
	// second claimant must still not see this row, because claimed_at
	// (the lease), not a SQL-level lock, is what protects it now.
	second, err := db.ClaimPendingBroadcastRecipients(ctx, b.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("a second claimant re-claimed an in-lease row: %+v", second)
	}
}

// TestClaimPendingBroadcastRecipientsReclaimsAfterLeaseExpires proves the
// flip side: a stale claim (crashed claimant) must eventually become
// reclaimable — this is what makes the lease a lease and not a permanent
// lock, i.e. no work is lost to a crash between claim and completion.
func TestClaimPendingBroadcastRecipientsReclaimsAfterLeaseExpires(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn, aud, tmpl := setupAudienceFixture(t, db)
	c := mustContact(t, db, tn.ID, "one@example.com")
	db.AddAudienceMember(ctx, tn.ID, aud.ID, c.ID)
	b := createBroadcast(t, db, tn, aud, tmpl)
	db.SnapshotBroadcastBatch(ctx, b, 10)

	first, err := db.ClaimPendingBroadcastRecipients(ctx, b.ID, 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("%v %v", first, err)
	}
	// Simulate the lease having expired (a crashed claimant that never
	// finished), by backdating the stamp — never by sleeping in the test.
	if _, err := db.pool.Exec(ctx, `UPDATE broadcast_recipients SET claimed_at = now() - interval '1 hour' WHERE id = $1`, first[0].ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := db.ClaimPendingBroadcastRecipients(ctx, b.ID, 10)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].ID != first[0].ID {
		t.Fatalf("expired lease was not reclaimed: %+v %v", reclaimed, err)
	}
}

// TestRecordBroadcastRecipientFailureTerminatesAfterMaxAttempts proves the
// second PR review fix: a deterministically-failing recipient does not
// retry (and get recharged against tenant quota) forever, and its
// broadcast can still complete once it is the only remaining "pending" row.
func TestRecordBroadcastRecipientFailureTerminatesAfterMaxAttempts(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn, aud, tmpl := setupAudienceFixture(t, db)
	c := mustContact(t, db, tn.ID, "one@example.com")
	db.AddAudienceMember(ctx, tn.ID, aud.ID, c.ID)
	b := createBroadcast(t, db, tn, aud, tmpl)
	db.MarkBroadcastExpanding(ctx, b.ID)
	db.SnapshotBroadcastBatch(ctx, b, 10)

	pending, err := db.ClaimPendingBroadcastRecipients(ctx, b.ID, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("%v %v", pending, err)
	}
	id := pending[0].ID
	for i := 0; i < maxRecipientAttempts-1; i++ {
		if err := db.RecordBroadcastRecipientFailure(ctx, id); err != nil {
			t.Fatal(err)
		}
		var status string
		db.pool.QueryRow(ctx, `SELECT status FROM broadcast_recipients WHERE id = $1`, id).Scan(&status)
		if status != "pending" {
			t.Fatalf("attempt %d: status = %q, want still pending (not yet at max attempts)", i+1, status)
		}
	}
	if err := db.RecordBroadcastRecipientFailure(ctx, id); err != nil {
		t.Fatal(err)
	}
	var status string
	var attempts int
	db.pool.QueryRow(ctx, `SELECT status, attempts FROM broadcast_recipients WHERE id = $1`, id).Scan(&status, &attempts)
	if status != "failed" || attempts != maxRecipientAttempts {
		t.Fatalf("status = %q attempts = %d, want failed/%d", status, attempts, maxRecipientAttempts)
	}

	// A 'failed' recipient is terminal, not pending, so the broadcast can
	// still complete rather than being blocked on it forever.
	if pending, _ := db.HasPendingBroadcastRecipients(ctx, b.ID); pending {
		t.Fatal("a failed recipient must not count as still pending")
	}
	completed, err := db.TryCompleteBroadcast(ctx, b.ID)
	if err != nil || !completed {
		t.Fatalf("broadcast should complete once its only recipient has terminally failed: %v %v", completed, err)
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
	pending, _ := db.ClaimPendingBroadcastRecipients(ctx, b.ID, 10)
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

// TestClaimActiveBroadcastsDueScanAtScale is EXPLAIN evidence (v0.37) that
// the new send_at gate on ClaimActiveBroadcasts does not degrade to a full
// scan once most active broadcasts are future-scheduled: idx_broadcasts_
// active_due (send_at, created_at) WHERE status IN (...) should let the
// planner find the few due rows without reading every not-yet-due one.
func TestClaimActiveBroadcastsDueScanAtScale(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn, aud, tmpl := setupAudienceFixture(t, db)
	future := time.Now().UTC().Add(24 * time.Hour)
	for i := 0; i < 2000; i++ {
		if _, err := db.CreateBroadcast(ctx, NewBroadcast{
			TenantID: tn.ID, AudienceID: aud.ID, TemplateID: tmpl.ID, Name: "bulk",
			FromAddress: "a@example.com", SubjectTemplate: "s", TextTemplate: "t", SendAt: &future,
		}); err != nil {
			t.Fatal(err)
		}
	}
	due := time.Now().UTC().Add(-time.Second)
	wantID := ""
	for i := 0; i < 3; i++ {
		b, err := db.CreateBroadcast(ctx, NewBroadcast{
			TenantID: tn.ID, AudienceID: aud.ID, TemplateID: tmpl.ID, Name: "due",
			FromAddress: "a@example.com", SubjectTemplate: "s", TextTemplate: "t", SendAt: &due,
		})
		if err != nil {
			t.Fatal(err)
		}
		wantID = b.ID
	}
	if _, err := db.pool.Exec(ctx, `ANALYZE broadcasts`); err != nil {
		t.Fatal(err)
	}

	claimed, err := db.ClaimActiveBroadcasts(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 3 {
		t.Fatalf("expected exactly the 3 due broadcasts, got %d", len(claimed))
	}
	var found bool
	for _, c := range claimed {
		if c.ID == wantID {
			found = true
		}
	}
	if !found {
		t.Fatal("due broadcast missing from claim")
	}

	rows, err := db.pool.Query(ctx, `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, BUFFERS) SELECT `+broadcastColumns+`
		FROM broadcasts WHERE status IN ('accepted','expanding') AND (send_at IS NULL OR send_at <= now())
		ORDER BY created_at LIMIT 10 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		_ = rows.Scan(&line)
		plan.WriteString(line + "\n")
	}
	t.Log("\n" + plan.String())
	if strings.Contains(plan.String(), "Seq Scan on broadcasts") {
		t.Fatalf("due-broadcast scan sequentially scans broadcasts at 2003 rows:\n%s", plan.String())
	}
}
