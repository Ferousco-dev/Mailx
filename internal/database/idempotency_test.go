package database

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

const testOp = "emails.create"

func TestClaimIdempotencyKeyFirstCallWins(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	now := time.Now().UTC()

	rec, owned, err := db.ClaimIdempotencyKey(ctx, tenant.ID, testOp, "k1", "fp1", now.Add(time.Hour), now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !owned {
		t.Fatal("expected the first claim to win")
	}
	if rec.Status != IdempotencyInProgress || rec.Fingerprint != "fp1" {
		t.Fatalf("unexpected claim state: %+v", rec)
	}
}

func TestClaimIdempotencyKeySecondCallLoses(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	now := time.Now().UTC()

	if _, owned, err := db.ClaimIdempotencyKey(ctx, tenant.ID, testOp, "k1", "fp1", now.Add(time.Hour), now.Add(-time.Minute)); err != nil || !owned {
		t.Fatalf("first claim: owned=%v err=%v", owned, err)
	}
	rec, owned, err := db.ClaimIdempotencyKey(ctx, tenant.ID, testOp, "k1", "fp1", now.Add(time.Hour), now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if owned {
		t.Fatal("expected the second claim to lose")
	}
	if rec.Status != IdempotencyInProgress {
		t.Fatalf("expected the existing in-progress record back, got %+v", rec)
	}
}

func TestClaimIdempotencyKeyStaleReclaim(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	// Staleness is judged by created_at, which we cannot backdate through
	// the public API, so instead prove reclaim using a staleCutoff that
	// is intentionally AFTER now (i.e. any created_at qualifies),
	// simulating "this claim is old enough".
	now := time.Now().UTC()
	if _, owned, err := db.ClaimIdempotencyKey(ctx, tenant.ID, testOp, "k1", "fp1", now.Add(time.Hour), now.Add(-time.Minute)); err != nil || !owned {
		t.Fatalf("first claim: owned=%v err=%v", owned, err)
	}

	// staleCutoff in the future relative to the claim's real created_at
	// means "anything created before now+1h counts as stale" - proving
	// the reclaim path fires and hands ownership to the new caller.
	rec, owned, err := db.ClaimIdempotencyKey(ctx, tenant.ID, testOp, "k1", "fp2", now.Add(2*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !owned {
		t.Fatal("expected a stale claim to be reclaimable")
	}
	if rec.Fingerprint != "fp2" {
		t.Fatalf("expected the reclaiming caller's fingerprint to win, got %q", rec.Fingerprint)
	}
}

func TestClaimIdempotencyKeyDoesNotReclaimFreshInProgress(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	now := time.Now().UTC()

	if _, owned, err := db.ClaimIdempotencyKey(ctx, tenant.ID, testOp, "k1", "fp1", now.Add(time.Hour), now.Add(-time.Minute)); err != nil || !owned {
		t.Fatalf("first claim: owned=%v err=%v", owned, err)
	}
	// staleCutoff in the past relative to created_at: this claim is NOT
	// stale, so a second caller must not reclaim it.
	rec, owned, err := db.ClaimIdempotencyKey(ctx, tenant.ID, testOp, "k1", "fp2", now.Add(time.Hour), now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if owned {
		t.Fatal("a fresh in-progress claim must not be reclaimable")
	}
	if rec.Fingerprint != "fp1" {
		t.Fatalf("expected the original owner's fingerprint, got %q", rec.Fingerprint)
	}
}

func TestClaimIdempotencyKeyTenantIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenantA := newTestTenant(t, db)
	tenantB, err := db.CreateTenant(ctx, "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	if _, owned, err := db.ClaimIdempotencyKey(ctx, tenantA.ID, testOp, "same-key", "fpA", now.Add(time.Hour), now.Add(-time.Minute)); err != nil || !owned {
		t.Fatalf("tenant A claim: owned=%v err=%v", owned, err)
	}
	if _, owned, err := db.ClaimIdempotencyKey(ctx, tenantB.ID, testOp, "same-key", "fpB", now.Add(time.Hour), now.Add(-time.Minute)); err != nil || !owned {
		t.Fatalf("tenant B must independently own the same literal key: owned=%v err=%v", owned, err)
	}
}

func TestGetIdempotencyKeyUnknownIsNotFound(t *testing.T) {
	db := newTestDB(t)
	tenant := newTestTenant(t, db)
	if _, err := db.GetIdempotencyKey(context.Background(), tenant.ID, testOp, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestInsertMessageCompletesIdempotencyClaimInSameTransaction(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	now := time.Now().UTC()

	if _, owned, err := db.ClaimIdempotencyKey(ctx, tenant.ID, testOp, "k1", "fp1", now.Add(time.Hour), now.Add(-time.Minute)); err != nil || !owned {
		t.Fatalf("claim: owned=%v err=%v", owned, err)
	}

	in := sampleNewMessage(t, tenant.ID)
	in.IdempotencyCompletion = &IdempotencyCompletion{Operation: testOp, IdempotencyKey: "k1", Fingerprint: "fp1"}
	msg, err := db.InsertMessage(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	rec, err := db.GetIdempotencyKey(ctx, tenant.ID, testOp, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != IdempotencyCompleted {
		t.Fatalf("expected completed, got %+v", rec)
	}
	if rec.ResourceID == nil || *rec.ResourceID != msg.ID {
		t.Fatalf("expected resource_id %s, got %+v", msg.ID, rec.ResourceID)
	}
}

// TestInsertMessageRollsBackIfIdempotencyClaimLost proves the crash-safety
// invariant: if the claim this message was meant to complete is no longer
// owned by it (already completed or reclaimed away), the WHOLE
// transaction rolls back - no message is left referring to a claim it
// does not actually own.
func TestInsertMessageRollsBackIfIdempotencyClaimLost(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	// No claim exists at all for "missing-key" - InsertMessage's guard
	// UPDATE will affect 0 rows.
	in := sampleNewMessage(t, tenant.ID)
	in.IdempotencyCompletion = &IdempotencyCompletion{Operation: testOp, IdempotencyKey: "missing-key"}
	if _, err := db.InsertMessage(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	if _, err := db.GetMessage(ctx, tenant.ID, in.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("message must not have been committed when its idempotency claim was lost")
	}
}

// TestInsertMessageRollsBackIfClaimWasReclaimedWithDifferentFingerprint
// is a regression test for a Greptile-flagged bug: a stalled claimant
// (A) whose claim gets reclaimed by a different caller (B, different
// payload/fingerprint) must NOT be able to complete using B's now-current
// row - that would mark the key completed under B's fingerprint but
// pointing at A's unrelated message, so a future replay for B's payload
// would incorrectly return A's message.
func TestInsertMessageRollsBackIfClaimWasReclaimedWithDifferentFingerprint(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	now := time.Now().UTC()

	// A claims first.
	if _, owned, err := db.ClaimIdempotencyKey(ctx, tenant.ID, testOp, "k1", "fpA", now.Add(time.Hour), now.Add(-time.Minute)); err != nil || !owned {
		t.Fatalf("A's claim: owned=%v err=%v", owned, err)
	}
	// B reclaims it as stale (staleCutoff in the future matches anything).
	if _, owned, err := db.ClaimIdempotencyKey(ctx, tenant.ID, testOp, "k1", "fpB", now.Add(time.Hour), now.Add(time.Hour)); err != nil || !owned {
		t.Fatalf("B's reclaim: owned=%v err=%v", owned, err)
	}

	// A, unaware it was reclaimed, finally finishes and tries to complete
	// using ITS OWN (now-stale) fingerprint.
	inA := sampleNewMessage(t, tenant.ID)
	inA.IdempotencyCompletion = &IdempotencyCompletion{Operation: testOp, IdempotencyKey: "k1", Fingerprint: "fpA"}
	if _, err := db.InsertMessage(ctx, inA); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected A's stale completion to be rejected with ErrConflict, got %v", err)
	}
	if _, err := db.GetMessage(ctx, tenant.ID, inA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("A's message must not have been committed")
	}

	// B (the rightful current owner) can still complete normally.
	inB := sampleNewMessage(t, tenant.ID)
	inB.IdempotencyCompletion = &IdempotencyCompletion{Operation: testOp, IdempotencyKey: "k1", Fingerprint: "fpB"}
	msgB, err := db.InsertMessage(ctx, inB)
	if err != nil {
		t.Fatalf("expected B's completion to succeed: %v", err)
	}

	rec, err := db.GetIdempotencyKey(ctx, tenant.ID, testOp, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Fingerprint != "fpB" || rec.ResourceID == nil || *rec.ResourceID != msgB.ID {
		t.Fatalf("expected the completed row to reflect B's fingerprint and message, got %+v", rec)
	}
}

func TestConcurrentClaimsExactlyOneWinner(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	now := time.Now().UTC()

	const attempts = 25
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, owned, err := db.ClaimIdempotencyKey(ctx, tenant.ID, testOp, "race-key", "fp", now.Add(time.Hour), now.Add(-time.Minute))
			if err != nil {
				t.Error(err)
				return
			}
			if owned {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("expected exactly 1 winner among %d concurrent claims, got %d", attempts, wins)
	}
}

func TestDeleteExpiredIdempotencyKeys(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	now := time.Now().UTC()

	if _, _, err := db.ClaimIdempotencyKey(ctx, tenant.ID, testOp, "expired", "fp", now.Add(-time.Minute), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClaimIdempotencyKey(ctx, tenant.ID, testOp, "active", "fp", now.Add(time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	n, err := db.DeleteExpiredIdempotencyKeys(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 expired row removed, got %d", n)
	}
	if _, err := db.GetIdempotencyKey(ctx, tenant.ID, testOp, "expired"); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired row should have been deleted")
	}
	if _, err := db.GetIdempotencyKey(ctx, tenant.ID, testOp, "active"); err != nil {
		t.Fatal("active row must not have been deleted")
	}
}
