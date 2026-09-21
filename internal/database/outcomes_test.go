package database

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPersistDeliveryOutcomeAtomicStates(t *testing.T) {
	tests := []struct {
		name      string
		decision  Decision
		accepted  bool
		kind      string
		wantState MessageStatus
		wantEvent EventType
	}{
		{"temporary", DecisionRetry, false, "transfer_temporary", StatusRetrying, EventDeferred},
		{"delivered", DecisionTerminalSuccess, true, "accepted", StatusDelivered, EventDelivered},
		{"failed", DecisionTerminalFailure, false, "transfer_permanent", StatusFailed, EventFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			ctx := context.Background()
			tenant := newTestTenant(t, db)
			msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
			if err != nil {
				t.Fatal(err)
			}
			attempt := sampleAttempt(msg.ID, 1, tc.decision)
			attempt.Accepted = tc.accepted
			attempt.Kind = tc.kind
			if tc.accepted {
				attempt.FinalCode = 250
			}
			if err := db.PersistDeliveryOutcome(ctx, attempt, false, map[string]any{"safe": "metadata"}); err != nil {
				t.Fatal(err)
			}

			state, err := db.LoadDeliveryState(ctx, msg.ID)
			if err != nil {
				t.Fatal(err)
			}
			if state.Status != tc.wantState || len(state.Attempts) != 1 {
				t.Fatalf("incoherent durable state: %+v", state)
			}
			events, err := db.ListMessageEvents(ctx, msg.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 2 || events[1].Type != tc.wantEvent || events[1].DeliveryAttemptNumber == nil || *events[1].DeliveryAttemptNumber != 1 {
				t.Fatalf("unexpected outcome events: %+v", events)
			}
			if tc.wantState == StatusDelivered && (state.Terminal() == false || state.Attempts[0].QuitError != nil) {
				// This case has no QUIT error; terminal truth must still be explicit.
				t.Fatalf("delivered state not terminal: %+v", state)
			}
		})
	}
}

func TestPersistDeliveryOutcomeIsIdempotentByAttempt(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	attempt := sampleAttempt(msg.ID, 1, DecisionTerminalSuccess)
	attempt.Accepted = true
	attempt.Kind = "accepted"
	attempt.FinalCode = 250
	for range 2 {
		if err := db.PersistDeliveryOutcome(ctx, attempt, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	state, err := db.LoadDeliveryState(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := db.ListMessageEvents(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Attempts) != 1 || len(events) != 2 {
		t.Fatalf("duplicate persistence duplicated truth: attempts=%d events=%d", len(state.Attempts), len(events))
	}

	second := attempt
	second.AttemptNumber = 2
	if err := db.PersistDeliveryOutcome(ctx, second, false, nil); !errors.Is(err, ErrMessageTerminal) {
		t.Fatalf("new attempt after delivered state must be blocked, got %v", err)
	}
}

func TestPersistDeliveryOutcomeAllowsRepeatedDeferredEvents(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 2; n++ {
		attempt := sampleAttempt(msg.ID, n, DecisionRetry)
		if err := db.PersistDeliveryOutcome(ctx, attempt, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	events, err := db.ListMessageEvents(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[1].Type != EventDeferred || events[2].Type != EventDeferred {
		t.Fatalf("legitimate delayed history was collapsed: %+v", events)
	}
}

func TestLoadDeliveryStateRestoresNextRetryAt(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	next := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Microsecond)
	if err := db.PersistDeliveryOutcome(ctx, sampleAttempt(msg.ID, 1, DecisionRetry), false, map[string]any{"next_retry_at": next}); err != nil {
		t.Fatal(err)
	}
	state, err := db.LoadDeliveryState(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.NextRetryAt == nil || !state.NextRetryAt.Equal(next) {
		t.Fatalf("next retry = %v, want %s", state.NextRetryAt, next)
	}
}

func TestPersistDeliveryOutcomeMarksRetryExhaustionFailed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	attempt := sampleAttempt(msg.ID, 1, DecisionRetry)
	if err := db.PersistDeliveryOutcome(ctx, attempt, true, nil); err != nil {
		t.Fatal(err)
	}
	state, err := db.LoadDeliveryState(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := db.ListMessageEvents(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusFailed || len(events) != 2 || events[1].Type != EventFailed {
		t.Fatalf("exhausted retry was not terminal failure: state=%+v events=%+v", state, events)
	}
}

func TestPersistDeliveryOutcomePreservesAcceptedQuitError(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	attempt := sampleAttempt(msg.ID, 1, DecisionTerminalSuccess)
	attempt.Accepted = true
	attempt.Kind = "accepted"
	attempt.FinalCode = 250
	attempt.QuitError = "connection reset during QUIT"
	if err := db.PersistDeliveryOutcome(ctx, attempt, false, nil); err != nil {
		t.Fatal(err)
	}
	state, err := db.LoadDeliveryState(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusDelivered || len(state.Attempts) != 1 || state.Attempts[0].QuitError == nil {
		t.Fatalf("QUIT failure changed accepted truth: %+v", state)
	}
}

func TestPersistDeliveryOutcomeRollsBackCoherently(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `
		CREATE FUNCTION reject_outcome_event() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.delivery_attempt_number IS NOT NULL THEN
				RAISE EXCEPTION 'controlled event insert failure';
			END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER reject_outcome_event BEFORE INSERT ON events
		FOR EACH ROW EXECUTE FUNCTION reject_outcome_event()`); err != nil {
		t.Fatal(err)
	}
	attempt := sampleAttempt(msg.ID, 1, DecisionTerminalSuccess)
	attempt.Accepted = true
	attempt.Kind = "accepted"
	attempt.FinalCode = 250
	if err := db.PersistDeliveryOutcome(ctx, attempt, false, nil); err == nil {
		t.Fatal("expected controlled transaction failure")
	}
	state, err := db.LoadDeliveryState(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := db.ListMessageEvents(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusQueued || len(state.Attempts) != 0 || len(events) != 1 || events[0].Type != EventQueued {
		t.Fatalf("failed transaction leaked partial truth: state=%+v events=%+v", state, events)
	}
}

func TestPersistDeliveryOutcomeConcurrentDuplicateIsSingleTruth(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	attempt := sampleAttempt(msg.ID, 1, DecisionTerminalSuccess)
	attempt.Accepted = true
	attempt.Kind = "accepted"
	attempt.FinalCode = 250
	const workers = 8
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- db.PersistDeliveryOutcome(ctx, attempt, false, nil)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent idempotent persistence failed: %v", err)
		}
	}
	state, err := db.LoadDeliveryState(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := db.ListMessageEvents(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Attempts) != 1 || len(events) != 2 || events[1].TenantID != tenant.ID {
		t.Fatalf("concurrent persistence duplicated or crossed tenant truth: state=%+v events=%+v", state, events)
	}
}
