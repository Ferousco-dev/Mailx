package database

import (
	"context"
	"errors"
	"testing"
	"time"
)

func sampleAttempt(messageID string, number int, decision Decision) NewDeliveryAttempt {
	start := time.Now().UTC()
	return NewDeliveryAttempt{
		MessageID:      messageID,
		AttemptNumber:  number,
		Decision:       decision,
		Kind:           "transfer_temporary",
		Accepted:       false,
		FinalCode:      451,
		EnhancedStatus: "4.3.0",
		RemoteMessage:  "greylisted",
		FailureStage:   "mail_from",
		MXAttempts: []MXAttempt{
			{Host: "mx1.example.com", Preference: 10, Destination: "mx1.example.com:25", Accepted: false, Code: 451, StartedAt: start, FinishedAt: start.Add(time.Millisecond)},
		},
		StartedAt:  start,
		FinishedAt: start.Add(2 * time.Millisecond),
	}
}

func TestInsertDeliveryAttemptRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}

	in := sampleAttempt(msg.ID, 1, DecisionRetry)
	got, err := db.InsertDeliveryAttempt(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == "" {
		t.Fatal("expected generated ID")
	}
	if got.FinalCode == nil || *got.FinalCode != 451 {
		t.Fatalf("final_code not preserved: %+v", got.FinalCode)
	}
	if len(got.MXAttempts) != 1 || got.MXAttempts[0].Host != "mx1.example.com" {
		t.Fatalf("mx_attempts not preserved: %+v", got.MXAttempts)
	}

	list, err := db.ListDeliveryAttempts(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].AttemptNumber != 1 {
		t.Fatalf("expected 1 attempt, got %+v", list)
	}
}

func TestInsertDeliveryAttemptDuplicateNumberIsConflict(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertDeliveryAttempt(ctx, sampleAttempt(msg.ID, 1, DecisionRetry)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertDeliveryAttempt(ctx, sampleAttempt(msg.ID, 1, DecisionRetry)); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict for duplicate (message_id, attempt_number), got %v", err)
	}
}

func TestInsertDeliveryAttemptOrderingByNumber(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	// Insert out of order; list must return them sorted by attempt_number.
	if _, err := db.InsertDeliveryAttempt(ctx, sampleAttempt(msg.ID, 2, DecisionRetry)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertDeliveryAttempt(ctx, sampleAttempt(msg.ID, 1, DecisionRetry)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertDeliveryAttempt(ctx, sampleAttempt(msg.ID, 3, DecisionTerminalFailure)); err != nil {
		t.Fatal(err)
	}
	list, err := db.ListDeliveryAttempts(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].AttemptNumber != 1 || list[1].AttemptNumber != 2 || list[2].AttemptNumber != 3 {
		t.Fatalf("expected ascending attempt_number order, got %+v", list)
	}
}

func TestInsertDeliveryAttemptRejectsInvalidInput(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}

	bad := sampleAttempt(msg.ID, 0, DecisionRetry) // invalid attempt number
	if _, err := db.InsertDeliveryAttempt(ctx, bad); err == nil {
		t.Fatal("expected error for non-positive attempt number")
	}

	bad2 := sampleAttempt(msg.ID, 1, Decision("bogus"))
	if _, err := db.InsertDeliveryAttempt(ctx, bad2); err == nil {
		t.Fatal("expected error for invalid decision")
	}

	bad3 := sampleAttempt(msg.ID, 1, DecisionRetry)
	bad3.FinishedAt = bad3.StartedAt.Add(-time.Second)
	if _, err := db.InsertDeliveryAttempt(ctx, bad3); err == nil {
		t.Fatal("expected error for finished before started")
	}
}

func TestInsertDeliveryAttemptAcceptedTerminalSuccess(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC()
	in := NewDeliveryAttempt{
		MessageID:     msg.ID,
		AttemptNumber: 1,
		Decision:      DecisionTerminalSuccess,
		Kind:          "accepted",
		Accepted:      true,
		FinalCode:     250,
		QuitError:     "connection reset during QUIT",
		StartedAt:     start,
		FinishedAt:    start.Add(time.Millisecond),
	}
	got, err := db.InsertDeliveryAttempt(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Accepted {
		t.Fatal("accepted flag not preserved")
	}
	if got.QuitError == nil || *got.QuitError != "connection reset during QUIT" {
		t.Fatalf("quit_error not preserved (accepted+QUIT-failure must remain visible): %+v", got.QuitError)
	}
}

// --------------------------------------------------------------- events -

func TestAppendEventAndListMessageEvents(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendEvent(ctx, tenant.ID, msg.ID, EventDeliveryAttempted, map[string]any{"attempt_number": float64(1), "code": float64(451)}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendEvent(ctx, tenant.ID, msg.ID, EventDelivered, nil); err != nil {
		t.Fatal(err)
	}
	events, err := db.ListMessageEvents(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	// InsertMessage already appended 1 "queued" event; +2 here = 3 total.
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0].Type != EventQueued || events[1].Type != EventDeliveryAttempted || events[2].Type != EventDelivered {
		t.Fatalf("events not in chronological order: %+v", events)
	}
	if events[1].Metadata["code"] != float64(451) {
		t.Fatalf("event metadata not preserved: %+v", events[1].Metadata)
	}
}

func TestAppendEventRejectsInvalidType(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendEvent(ctx, tenant.ID, msg.ID, EventType("bogus"), nil); err == nil {
		t.Fatal("expected error for invalid event type")
	}
}

func TestListTenantEventsIsolationAndOrdering(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenantA := newTestTenant(t, db)
	tenantB, err := db.CreateTenant(ctx, "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	msgA, err := db.InsertMessage(ctx, sampleNewMessage(t, tenantA.ID))
	if err != nil {
		t.Fatal(err)
	}
	msgB, err := db.InsertMessage(ctx, sampleNewMessage(t, tenantB.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendEvent(ctx, tenantA.ID, msgA.ID, EventDelivered, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendEvent(ctx, tenantB.ID, msgB.ID, EventDelivered, nil); err != nil {
		t.Fatal(err)
	}

	eventsA, err := db.ListTenantEvents(ctx, tenantA.ID, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range eventsA {
		if e.TenantID != tenantA.ID {
			t.Fatalf("tenant isolation violated in event listing: got tenant %q", e.TenantID)
		}
	}
	// tenantA has 2 events (queued from InsertMessage + delivered).
	if len(eventsA) != 2 {
		t.Fatalf("expected 2 events for tenant A, got %d", len(eventsA))
	}
}

func TestDeliveryAttemptCascadeDeleteWithMessage(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertDeliveryAttempt(ctx, sampleAttempt(msg.ID, 1, DecisionRetry)); err != nil {
		t.Fatal(err)
	}
	// Deleting the message (direct SQL — no repository method exposes
	// this yet, deliberately; see report) must cascade to recipients,
	// delivery_attempts, and events per the migration's FK definitions.
	if _, err := db.pool.Exec(ctx, `DELETE FROM messages WHERE id = $1`, msg.ID); err != nil {
		t.Fatal(err)
	}
	list, err := db.ListDeliveryAttempts(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected delivery_attempts to cascade-delete with message, got %d rows", len(list))
	}
	recipients, err := db.ListRecipients(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recipients) != 0 {
		t.Fatalf("expected recipients to cascade-delete with message, got %d rows", len(recipients))
	}
}

func TestTenantDeleteRestrictedWhileMessagesExist(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	if _, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID)); err != nil {
		t.Fatal(err)
	}
	// ON DELETE RESTRICT on messages.tenant_id must prevent deleting a
	// tenant that still owns messages — audit/history is never silently
	// destroyed by a tenant deletion.
	_, err := db.pool.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenant.ID)
	if err == nil {
		t.Fatal("expected FK RESTRICT violation deleting a tenant with messages")
	}
}
