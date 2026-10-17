package broadcast

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
)

func TestCancelledBroadcastStalePollerDoesNotSnapshotOrQueue(t *testing.T) {
	db := newTestDB(t)
	e := newTestExpander(t, db)
	ctx := context.Background()
	tn, b := setupBroadcastReady(t, db, 3)
	claimed, err := db.ClaimActiveBroadcasts(ctx, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if _, err := db.CancelBroadcast(ctx, tn.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	e.process(ctx, claimed[0])
	runTicks(e, ctx, 3)
	got, err := db.GetBroadcast(ctx, tn.ID, b.ID)
	if err != nil || got.Status != "cancelled" {
		t.Fatalf("stale poller resumed: %+v %v", got, err)
	}
	if rows, err := db.ListBroadcastRecipients(ctx, tn.ID, b.ID, 10, nil); err != nil || len(rows) != 0 {
		t.Fatalf("cancelled audience was snapshotted: %+v %v", rows, err)
	}
}

func TestRescheduledBroadcastStalePollerDoesNotStartEarly(t *testing.T) {
	db := newTestDB(t)
	e := newTestExpander(t, db)
	ctx := context.Background()
	tn, b := setupBroadcastReady(t, db, 1)
	if _, err := db.RescheduleBroadcast(ctx, tn.ID, b.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	e.process(ctx, b)
	runTicks(e, ctx, 3)
	got, err := db.GetBroadcast(ctx, tn.ID, b.ID)
	if err != nil || got.Status != "accepted" || got.SnapshotComplete {
		t.Fatalf("old schedule activated: %+v %v", got, err)
	}
}

func TestCancelledBroadcastClaimedRecipientsDoNotMaterialize(t *testing.T) {
	db := newTestDB(t)
	e := newTestExpander(t, db)
	ctx := context.Background()
	tn, b := setupBroadcastReady(t, db, 3)
	if err := db.MarkBroadcastExpanding(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.SnapshotBroadcastBatch(ctx, b, 10); err != nil {
		t.Fatal(err)
	}
	pending, err := db.ClaimPendingBroadcastRecipients(ctx, b.ID, 10)
	if err != nil || len(pending) != 3 {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	if err := e.materializeOne(ctx, b, "example.com", pending[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CancelBroadcast(ctx, tn.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	e.materializeBatch(ctx, b, "example.com", pending[1:])
	for _, r := range pending[1:] {
		if _, err := db.GetMessage(ctx, tn.ID, r.ID); !errors.Is(err, database.ErrNotFound) {
			t.Fatalf("cancelled message exists: %v", err)
		}
		if _, err := e.store.Load(r.ID); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cancelled raw file remains: %v", err)
		}
	}
	if err := e.materializeOne(ctx, b, "example.com", pending[0]); err != nil {
		t.Fatalf("replaying accepted recipient: %v", err)
	}
	if _, err := e.store.Load(pending[0].ID); err != nil {
		t.Fatalf("cancellation removed already queued raw mail: %v", err)
	}
	stats, err := db.BroadcastAnalytics(ctx, tn.ID, b.ID)
	if err != nil || stats.Materialized != 1 || stats.Cancelled != 2 || stats.Pending != 0 {
		t.Fatalf("counts: %+v %v", stats, err)
	}
}

func TestCancelledBroadcastKeepsPartialSnapshotAndStopsNextBatch(t *testing.T) {
	db := newTestDB(t)
	e := newTestExpander(t, db)
	ctx := context.Background()
	tn, b := setupBroadcastReady(t, db, defaultSnapshotBatch+30)
	e.tick(ctx)
	before, err := db.BroadcastAnalytics(ctx, tn.ID, b.ID)
	if err != nil || before.Intended != defaultSnapshotBatch || before.Materialized != defaultMaterializeBatch {
		t.Fatalf("initial bounded tick: %+v %v", before, err)
	}
	if _, err := db.CancelBroadcast(ctx, tn.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	runTicks(e, ctx, 3)
	stats, err := db.BroadcastAnalytics(ctx, tn.ID, b.ID)
	if err != nil || stats.Intended != before.Intended || stats.Materialized != before.Materialized || stats.Pending != 0 || stats.Cancelled != defaultSnapshotBatch-defaultMaterializeBatch {
		t.Fatalf("cancellation expanded the unsnapshotted audience or handed off more messages: %+v %v", stats, err)
	}
}
