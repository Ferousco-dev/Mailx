package database

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInsertMessageCreatesOutboxRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	in := sampleNewMessage(t, tenant.ID)
	msg, err := db.InsertMessage(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	items, err := db.ListPendingOutbox(ctx, time.Now().Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, item := range items {
		if item.MessageID == msg.ID {
			found = true
			if item.TenantID != tenant.ID {
				t.Fatalf("wrong tenant on outbox row: %+v", item)
			}
		}
	}
	if !found {
		t.Fatalf("expected an outbox row for %s, got %+v", msg.ID, items)
	}
}

func TestListPendingOutboxExcludesFutureAvailableAt(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	in := sampleNewMessage(t, tenant.ID)
	in.AvailableAt = time.Now().Add(time.Hour)
	msg, err := db.InsertMessage(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	items, err := db.ListPendingOutbox(ctx, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.MessageID == msg.ID {
			t.Fatalf("scheduled-future message must not be listed as due yet: %+v", item)
		}
	}

	items, err = db.ListPendingOutbox(ctx, time.Now().Add(2*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, item := range items {
		if item.MessageID == msg.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the scheduled message to become due once its time has passed")
	}
}

func TestMarkOutboxDispatchedIsIdempotentGuard(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	in := sampleNewMessage(t, tenant.ID)
	msg, err := db.InsertMessage(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.MarkOutboxDispatched(ctx, msg.ID); err != nil {
		t.Fatal(err)
	}
	items, err := db.ListPendingOutbox(ctx, time.Now().Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.MessageID == msg.ID {
			t.Fatalf("dispatched row must not be listed as pending again: %+v", item)
		}
	}

	// Redispatch after a crash-before-mark scenario calls this again; it
	// must fail cleanly (not silently succeed twice, not panic) so the
	// caller can tell "already handled" from "row genuinely missing".
	if err := db.MarkOutboxDispatched(ctx, msg.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on second mark, got %v", err)
	}
}

func TestMarkOutboxDispatchedUnknownMessageIsNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := db.MarkOutboxDispatched(ctx, "does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
