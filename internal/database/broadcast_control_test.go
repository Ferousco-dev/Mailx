package database

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func controlBroadcast(t *testing.T, db *DB, count int) (Tenant, Broadcast, []BroadcastRecipient) {
	t.Helper()
	ctx := context.Background()
	tn, aud, tmpl := setupAudienceFixture(t, db)
	for i := 0; i < count; i++ {
		c := mustContact(t, db, tn.ID, fmt.Sprintf("control%d@example.com", i))
		if err := db.AddAudienceMember(ctx, tn.ID, aud.ID, c.ID); err != nil {
			t.Fatal(err)
		}
	}
	b := createBroadcast(t, db, tn, aud, tmpl)
	if err := db.MarkBroadcastExpanding(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.SnapshotBroadcastBatch(ctx, b, count+1); err != nil {
		t.Fatal(err)
	}
	recipients, err := db.ListBroadcastRecipients(ctx, tn.ID, b.ID, count+1, nil)
	if err != nil || len(recipients) != count {
		t.Fatalf("recipients: %+v %v", recipients, err)
	}
	return tn, b, recipients
}

func controlMessage(t *testing.T, tn Tenant, r BroadcastRecipient) NewMessage {
	t.Helper()
	in := sampleNewMessage(t, tn.ID)
	in.ID = r.ID
	in.RequireBroadcastRecipientID = r.ID
	in.Recipients = []RecipientInput{{Address: "<" + r.Email + ">", HeaderKind: strPtr("to")}}
	return in
}

func TestBroadcastCancelStopsHandoffsAndPreservesQueuedMessages(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn, b, recipients := controlBroadcast(t, db, 3)
	if _, err := db.InsertMessage(ctx, controlMessage(t, tn, recipients[0])); err != nil {
		t.Fatal(err)
	}
	cancelled, err := db.CancelBroadcast(ctx, tn.ID, b.ID)
	if err != nil || cancelled.Status != "cancelled" {
		t.Fatalf("cancel: %+v %v", cancelled, err)
	}
	replay, err := db.CancelBroadcast(ctx, tn.ID, b.ID)
	if err != nil || !replay.UpdatedAt.Equal(cancelled.UpdatedAt) {
		t.Fatalf("repeat cancellation changed the result: %+v %v", replay, err)
	}
	for _, r := range recipients[1:] {
		if _, err := db.InsertMessage(ctx, controlMessage(t, tn, r)); !errors.Is(err, ErrBroadcastStateConflict) {
			t.Fatalf("late handoff: %v", err)
		}
	}
	if _, err := db.InsertMessage(ctx, controlMessage(t, tn, recipients[0])); !errors.Is(err, ErrConflict) {
		t.Fatalf("already queued message must remain an idempotent conflict: %v", err)
	}
	if _, _, err := db.SnapshotBroadcastBatch(ctx, b, 10); !errors.Is(err, ErrBroadcastStateConflict) {
		t.Fatalf("stale snapshot: %v", err)
	}
	if claimed, err := db.ClaimPendingBroadcastRecipients(ctx, b.ID, 10); err != nil || len(claimed) != 0 {
		t.Fatalf("cancelled broadcast claimed: %+v %v", claimed, err)
	}
	if done, err := db.TryCompleteBroadcast(ctx, b.ID); err != nil || done {
		t.Fatalf("cancelled broadcast completed: %v %v", done, err)
	}
	listed, err := db.ListBroadcastRecipients(ctx, tn.ID, b.ID, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if listed[0].Status != "materialized" || listed[0].MessageID == nil {
		t.Fatalf("queued recipient lost: %+v", listed[0])
	}
	for _, r := range listed[1:] {
		if r.Status != "cancelled" || r.MessageID != nil {
			t.Fatalf("unqueued recipient: %+v", r)
		}
	}
	stats, err := db.BroadcastAnalytics(ctx, tn.ID, b.ID)
	if err != nil || stats.Intended != 3 || stats.Materialized != 1 || stats.Cancelled != 2 || stats.Pending != 0 {
		t.Fatalf("analytics: %+v %v", stats, err)
	}
	for _, table := range []string{"messages", "outbox", "events"} {
		var count int
		if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE tenant_id = $1", tn.ID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s: %d %v", table, count, err)
		}
	}
}

func TestBroadcastControlTenantIsolationAndTerminalStates(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn, _, b := setupBroadcastFixture(t, db)
	other := newTestTenant(t, db)
	for _, id := range []string{b.ID, "missing"} {
		if _, err := db.CancelBroadcast(ctx, other.ID, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign cancel: %v", err)
		}
		if _, err := db.RescheduleBroadcast(ctx, other.ID, id, time.Now().Add(time.Hour)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign reschedule: %v", err)
		}
	}
	for _, state := range []string{"completed", "failed"} {
		if _, err := db.pool.Exec(ctx, `UPDATE broadcasts SET status = $2 WHERE id = $1`, b.ID, state); err != nil {
			t.Fatal(err)
		}
		if _, err := db.CancelBroadcast(ctx, tn.ID, b.ID); !errors.Is(err, ErrBroadcastStateConflict) {
			t.Fatalf("cancel %s: %v", state, err)
		}
		if _, err := db.RescheduleBroadcast(ctx, tn.ID, b.ID, time.Now().Add(time.Hour)); !errors.Is(err, ErrBroadcastStateConflict) {
			t.Fatalf("reschedule %s: %v", state, err)
		}
	}
}

func TestBroadcastRescheduleFencesStaleActivationAndKeepsSnapshots(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn, _, b := setupBroadcastFixture(t, db)
	claimed, err := db.ClaimActiveBroadcasts(ctx, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("initial claim: %+v %v", claimed, err)
	}
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	rescheduled, err := db.RescheduleBroadcast(ctx, tn.ID, b.ID, future)
	if err != nil || rescheduled.SendAt == nil || !rescheduled.SendAt.Equal(future) {
		t.Fatalf("reschedule: %+v %v", rescheduled, err)
	}
	if !rescheduled.AudienceSnapshotAt.Equal(b.AudienceSnapshotAt) || rescheduled.SubjectTemplate != b.SubjectTemplate {
		t.Fatal("rescheduling changed the acceptance snapshots")
	}
	if err := db.MarkBroadcastExpanding(ctx, claimed[0].ID); !errors.Is(err, ErrBroadcastStateConflict) {
		t.Fatalf("stale activation: %v", err)
	}
	if _, _, err := db.SnapshotBroadcastBatch(ctx, claimed[0], 10); !errors.Is(err, ErrBroadcastStateConflict) {
		t.Fatalf("early snapshot: %v", err)
	}
	if due, err := db.ClaimActiveBroadcasts(ctx, 10); err != nil || len(due) != 0 {
		t.Fatalf("future broadcast claimed: %+v %v", due, err)
	}
	if _, err := db.RescheduleBroadcast(ctx, tn.ID, b.ID, time.Now().Add(-time.Hour)); !errors.Is(err, ErrInvalidBroadcastSchedule) {
		t.Fatalf("past schedule: %v", err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE broadcasts SET send_at = now() - interval '1 second' WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkBroadcastExpanding(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RescheduleBroadcast(ctx, tn.ID, b.ID, future); !errors.Is(err, ErrBroadcastStateConflict) {
		t.Fatalf("rescheduled after activation: %v", err)
	}
}

func waitForBlockedQuery(t *testing.T, db *DB, blocker int, query string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		var pid int
		err := db.pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
			WHERE $1 = ANY(pg_blocking_pids(pid)) AND strpos(query, $2) > 0 LIMIT 1`, blocker, query).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(normalizeErr(err), ErrNotFound) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("query %q did not wait on transaction %d", query, blocker)
	return 0
}

func TestBroadcastCancelWaitsForMessageHandoffCommit(t *testing.T) {
	db := newTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tn, b, recipients := controlBroadcast(t, db, 2)
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	var blockerPID int
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM broadcast_recipients WHERE id = $1 FOR UPDATE`, recipients[0].ID); err != nil {
		t.Fatal(err)
	}
	in := controlMessage(t, tn, recipients[0])
	inserted := make(chan error, 1)
	go func() { _, err := db.InsertMessage(ctx, in); inserted <- err }()
	insertPID := waitForBlockedQuery(t, db, blockerPID, "SELECT status FROM broadcast_recipients")
	cancelled := make(chan error, 1)
	go func() { _, err := db.CancelBroadcast(ctx, tn.ID, b.ID); cancelled <- err }()
	waitForBlockedQuery(t, db, insertPID, "FROM broadcasts")
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-inserted; err != nil {
		t.Fatal(err)
	}
	if err := <-cancelled; err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertMessage(ctx, controlMessage(t, tn, recipients[1])); !errors.Is(err, ErrBroadcastStateConflict) {
		t.Fatalf("handoff after cancellation returned: %v", err)
	}
	stats, err := db.BroadcastAnalytics(ctx, tn.ID, b.ID)
	if err != nil || stats.Materialized != 1 || stats.Cancelled != 1 {
		t.Fatalf("post-race counts: %+v %v", stats, err)
	}
}

func TestBroadcastCancellationWinsBeforeBlockedMessageInsert(t *testing.T) {
	db := newTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tn, b, recipients := controlBroadcast(t, db, 1)
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE broadcasts SET status = 'cancelled' WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	in := controlMessage(t, tn, recipients[0])
	done := make(chan error, 1)
	go func() { _, err := db.InsertMessage(ctx, in); done <- err }()
	waitForBlockedQuery(t, db, pid, "send_at IS NULL OR send_at <= clock_timestamp() FROM broadcasts")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrBroadcastStateConflict) {
		t.Fatalf("insert ignored concurrent cancellation: %v", err)
	}
	if _, err := db.GetMessage(ctx, tn.ID, in.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancelled message exists: %v", err)
	}
}

func TestBroadcastCancelledLegacyHandoffIsReportedAsMaterialized(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn, b, recipients := controlBroadcast(t, db, 1)
	in := controlMessage(t, tn, recipients[0])
	in.RequireBroadcastRecipientID = ""
	if _, err := db.InsertMessage(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CancelBroadcast(ctx, tn.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	listed, err := db.ListBroadcastRecipients(ctx, tn.ID, b.ID, 10, nil)
	if err != nil || len(listed) != 1 || listed[0].Status != "materialized" || listed[0].MessageID == nil {
		t.Fatalf("legacy pending row with a committed message: %+v %v", listed, err)
	}
	stats, err := db.BroadcastAnalytics(ctx, tn.ID, b.ID)
	if err != nil || stats.Materialized != 1 || stats.Cancelled != 0 {
		t.Fatalf("legacy counts: %+v %v", stats, err)
	}
}

func TestBroadcastControlMigrationRollbackDoesNotReactivate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn, _, b := setupBroadcastFixture(t, db)
	if _, err := db.CancelBroadcast(ctx, tn.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateDownOne(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetBroadcast(ctx, tn.ID, b.ID)
	if err != nil || got.Status != "failed" || got.FailureReason != "cancelled" {
		t.Fatalf("rollback lost cancellation: %+v %v", got, err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if active, err := db.ClaimActiveBroadcasts(ctx, 10); err != nil || len(active) != 0 {
		t.Fatalf("rollback reactivated broadcast: %+v %v", active, err)
	}
}

func TestBroadcastRescheduleWinsAgainstBlockedActivation(t *testing.T) {
	db := newTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tn, _, b := setupBroadcastFixture(t, db)
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE broadcasts SET send_at = now() + interval '1 hour' WHERE id = $1 AND tenant_id = $2`, b.ID, tn.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- db.MarkBroadcastExpanding(ctx, b.ID) }()
	waitForBlockedQuery(t, db, pid, "UPDATE broadcasts SET status = 'expanding'")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrBroadcastStateConflict) {
		t.Fatalf("activation ignored concurrently changed schedule: %v", err)
	}
	got, err := db.GetBroadcast(ctx, tn.ID, b.ID)
	if err != nil || got.Status != "accepted" {
		t.Fatalf("rescheduled broadcast started: %+v %v", got, err)
	}
}
