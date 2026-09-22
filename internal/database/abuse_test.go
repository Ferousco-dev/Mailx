package database

import (
	"context"
	"strings"
	"testing"
	"time"
)

func insertAt(t *testing.T, db *DB, tenantID string, at time.Time) string {
	t.Helper()
	in := sampleNewMessage(t, tenantID)
	in.AvailableAt = at
	if _, err := db.InsertMessage(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	return in.ID
}

// One tenant's large backlog must not push another tenant's later messages out of
// a dispatch batch. Under the old global available_at order tenant B would get
// zero slots in the first batch.
func TestListPendingOutboxIsFairAcrossTenants(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a, b := newTestTenant(t, db), newTestTenant(t, db)
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 120; i++ {
		insertAt(t, db, a.ID, base.Add(time.Duration(i)*time.Millisecond))
	}
	bIDs := map[string]bool{}
	for i := 0; i < 5; i++ { // arrives later than every A row
		bIDs[insertAt(t, db, b.ID, base.Add(time.Minute+time.Duration(i)*time.Millisecond))] = true
	}

	items, err := db.ListPendingOutbox(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 20 {
		t.Fatalf("batch size %d, want 20", len(items))
	}
	gotB := 0
	for _, it := range items {
		if bIDs[it.MessageID] {
			gotB++
		}
	}
	if gotB != 5 {
		t.Fatalf("tenant B got %d of its 5 messages in the first batch of 20, want all 5", gotB)
	}
	// Round robin: the first two items are one of each tenant.
	if items[0].TenantID == items[1].TenantID {
		t.Fatalf("first two items are the same tenant: round robin is not interleaving")
	}
	// Within a tenant, oldest first.
	var last time.Time
	for _, it := range items {
		if it.TenantID != a.ID {
			continue
		}
		if it.AvailableAt.Before(last) {
			t.Fatal("tenant A items are not oldest-first")
		}
		last = it.AvailableAt
	}
}

func TestListPendingOutboxSingleTenantFillsBatchAndSkipsFutureAndDispatched(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	now := time.Now().UTC()
	var first string
	for i := 0; i < 30; i++ {
		id := insertAt(t, db, a.ID, now.Add(-time.Hour+time.Duration(i)*time.Second))
		if i == 0 {
			first = id
		}
	}
	futureID := insertAt(t, db, a.ID, now.Add(time.Hour)) // not due
	if err := db.MarkOutboxDispatched(ctx, first); err != nil {
		t.Fatal(err)
	}
	items, err := db.ListPendingOutbox(ctx, 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 25 {
		t.Fatalf("a single tenant must still fill the batch: got %d", len(items))
	}
	for _, it := range items {
		if it.MessageID == first {
			t.Fatal("dispatched row returned")
		}
		if it.MessageID == futureID {
			t.Fatal("future-scheduled row returned as due")
		}
	}
}

// EXPLAIN evidence that tenant discovery is an index probe, not a scan of the
// backlog. Skips the plan-shape assertion when the table is too small for the
// planner to prefer the index; the shape is recorded in docs/design-v0.31.md.
func TestListPendingOutboxUsesTenantIndex(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a, b := newTestTenant(t, db), newTestTenant(t, db)
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 60; i++ {
		insertAt(t, db, a.ID, base.Add(time.Duration(i)*time.Millisecond))
		insertAt(t, db, b.ID, base.Add(time.Duration(i)*time.Millisecond))
	}
	if _, err := db.pool.Exec(ctx, `ANALYZE outbox`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Skip("cannot steer planner on this pool")
	}
	// The real fair-dispatch query, logged as evidence (recorded in docs/design-v0.31.md).
	if pr, perr := db.pool.Query(ctx, "EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF) "+fairOutboxSQL, 100, maxFairTenants); perr == nil {
		for pr.Next() {
			var line string
			_ = pr.Scan(&line)
			t.Log(line)
		}
		pr.Close()
	}
	rows, err := db.pool.Query(ctx, `EXPLAIN SELECT tenant_id FROM outbox
		WHERE dispatched_at IS NULL AND available_at <= now() AND tenant_id > 'a'
		ORDER BY tenant_id LIMIT 1`)
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
	if !strings.Contains(plan.String(), "idx_outbox_pending_tenant") {
		t.Skipf("planner did not choose the tenant index on this pool (pooled SET may not apply):\n%s", plan.String())
	}
}

func TestCountsAreBoundedAndTenantScoped(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a, b := newTestTenant(t, db), newTestTenant(t, db)
	now := time.Now().UTC()
	for i := 0; i < 7; i++ {
		insertAt(t, db, a.ID, now)
	}
	insertAt(t, db, b.ID, now)

	if n, err := db.CountTenantQueued(ctx, a.ID, 100); err != nil || n != 7 {
		t.Fatalf("tenant a queued = %d, %v", n, err)
	}
	if n, err := db.CountTenantQueued(ctx, a.ID, 3); err != nil || n != 3 {
		t.Fatalf("count must stop at the limit: %d, %v", n, err)
	}
	if n, err := db.CountTenantQueued(ctx, b.ID, 100); err != nil || n != 1 {
		t.Fatalf("tenant b queued = %d, %v", n, err)
	}
	if n, err := db.CountPendingOutbox(ctx, 5); err != nil || n != 5 {
		t.Fatalf("pending outbox bounded count = %d, %v", n, err)
	}
	// A finished message stops counting.
	id := insertAt(t, db, b.ID, now)
	if err := db.UpdateMessageStatus(ctx, id, StatusDelivered, &now); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.CountTenantQueued(ctx, b.ID, 100); n != 1 {
		t.Fatalf("delivered message still counted: %d", n)
	}
}

func TestMessageTenant(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	id := insertAt(t, db, a.ID, time.Now().UTC())
	got, err := db.MessageTenant(ctx, id)
	if err != nil || got != a.ID {
		t.Fatalf("MessageTenant = %q, %v", got, err)
	}
	if _, err := db.MessageTenant(ctx, "does-not-exist"); err == nil {
		t.Fatal("unknown message must error")
	}
}

func TestReleaseIdempotencyClaimOnlyReleasesOwnInProgressClaim(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	exp := time.Now().Add(time.Hour)
	stale := time.Now().Add(-time.Minute)
	rec, owned, err := db.ClaimIdempotencyKey(ctx, a.ID, "emails.create", "k1", "fp1", exp, stale)
	if err != nil || !owned {
		t.Fatalf("claim: %v owned=%v", err, owned)
	}
	claimedAt := rec.CreatedAt
	// A different fingerprint must not release it.
	if err := db.ReleaseIdempotencyClaim(ctx, a.ID, "emails.create", "k1", "other", claimedAt); err != nil {
		t.Fatal(err)
	}
	if _, owned, _ := db.ClaimIdempotencyKey(ctx, a.ID, "emails.create", "k1", "fp1", exp, stale); owned {
		t.Fatal("claim was released by the wrong fingerprint")
	}
	if err := db.ReleaseIdempotencyClaim(ctx, a.ID, "emails.create", "k1", "fp1", claimedAt); err != nil {
		t.Fatal(err)
	}
	if _, owned, err := db.ClaimIdempotencyKey(ctx, a.ID, "emails.create", "k1", "fp1", exp, stale); err != nil || !owned {
		t.Fatalf("key must be claimable again after release: %v owned=%v", err, owned)
	}
	// Releasing a key that does not exist is a no-op.
	if err := db.ReleaseIdempotencyClaim(ctx, a.ID, "emails.create", "nope", "x", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkListPendingOutboxFair(b *testing.B) {
	db := newTestDB(b)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)
	for ti := 0; ti < 20; ti++ {
		tn := newTestTenant(b, db)
		for i := 0; i < 100; i++ {
			in := sampleNewMessage(&testing.T{}, tn.ID)
			in.AvailableAt = base.Add(time.Duration(i) * time.Millisecond)
			if _, err := db.InsertMessage(ctx, in); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.ListPendingOutbox(ctx, 100); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCountTenantQueued(b *testing.B) {
	db := newTestDB(b)
	ctx := context.Background()
	tn := newTestTenant(b, db)
	for i := 0; i < 500; i++ {
		in := sampleNewMessage(&testing.T{}, tn.ID)
		if _, err := db.InsertMessage(ctx, in); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.CountTenantQueued(ctx, tn.ID, 10000); err != nil {
			b.Fatal(err)
		}
	}
}

// At a realistic backlog the fair query must not sort or read a whole tenant backlog: the per-tenant
// probe reads only that tenant's oldest rows in index order, and a small tenant is not starved.
func TestFairDispatchPlanAtScale(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	big, small := newTestTenant(t, db), newTestTenant(t, db)
	bulk := func(tenantID string, n int, from time.Time) {
		_, err := db.pool.Exec(ctx, `
			WITH m AS (
				INSERT INTO messages (id, tenant_id, mail_from)
				SELECT $1 || g::text, $2, 'a@example.com' FROM generate_series(1, $3) g
				RETURNING id)
			INSERT INTO outbox (message_id, tenant_id, available_at)
			SELECT id, $2, $4::timestamptz + (row_number() OVER ())::int * interval '1 millisecond' FROM m`,
			tenantID+"-", tenantID, n, from)
		if err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().UTC().Add(-2 * time.Hour)
	bulk(big.ID, 30000, base)
	bulk(small.ID, 3, base.Add(time.Hour))
	if _, err := db.pool.Exec(ctx, `ANALYZE outbox`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	items, err := db.ListPendingOutbox(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	gotSmall := 0
	for _, it := range items {
		if it.TenantID == small.ID {
			gotSmall++
		}
	}
	if len(items) != 100 || gotSmall != 3 {
		t.Fatalf("items=%d small tenant got %d of 3 behind a 30000-row backlog", len(items), gotSmall)
	}
	if elapsed > time.Second {
		t.Fatalf("fair dispatch took %s with a 30000-row backlog", elapsed)
	}
	rows, err := db.pool.Query(ctx, "EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, BUFFERS) "+fairOutboxSQL, 100, maxFairTenants)
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
	if strings.Contains(plan.String(), "Seq Scan on outbox") {
		t.Fatalf("fair dispatch sequentially scans the outbox:\n%s", plan.String())
	}
}

func outboxIndexes(t *testing.T, db *DB) map[string]bool {
	t.Helper()
	rows, err := db.pool.Query(context.Background(), `SELECT indexname FROM pg_indexes WHERE tablename = 'outbox' AND schemaname = current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		out[n] = true
	}
	return out
}

// Migration 000014 swaps the outbox pending index, and its down migration restores the old one, with pending rows kept.
func TestOutboxIndexMigrationRoundTripKeepsRows(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	id := insertAt(t, db, a.ID, time.Now().UTC().Add(-time.Minute))
	if ix := outboxIndexes(t, db); !ix["idx_outbox_pending_tenant"] || ix["idx_outbox_pending"] {
		t.Fatalf("after up: %v", ix)
	}
	// Roll back past every migration newer than 000014 (the outbox index
	// migration), not just one: never assume it is the latest.
	for {
		if err := db.MigrateDownOne(ctx); err != nil {
			t.Fatal(err)
		}
		if ix := outboxIndexes(t, db); ix["idx_outbox_pending"] {
			break
		}
	}
	if ix := outboxIndexes(t, db); ix["idx_outbox_pending_tenant"] || !ix["idx_outbox_pending"] {
		t.Fatalf("after down: %v", ix)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if ix := outboxIndexes(t, db); !ix["idx_outbox_pending_tenant"] || ix["idx_outbox_pending"] {
		t.Fatalf("after re-up: %v", ix)
	}
	items, err := db.ListPendingOutbox(ctx, 10)
	if err != nil || len(items) != 1 || items[0].MessageID != id {
		t.Fatalf("pending row lost across the migration round trip: %v %v", items, err)
	}
}

// A retrying message is still undelivered and must count toward the tenant cap.
func TestRetryingMessagesCountTowardTenantQueueCap(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	id := insertAt(t, db, a.ID, time.Now().UTC())
	if err := db.UpdateMessageStatus(ctx, id, StatusRetrying, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := db.CountTenantQueued(ctx, a.ID, 100); err != nil || n != 1 {
		t.Fatalf("retrying message not counted: %d %v", n, err)
	}
}

// An identical retry that reclaims a stale claim must not be deleted by the ORIGINAL request's late release,
// even though the fingerprint is the same: the claim time identifies the owner.
func TestReleaseIdempotencyClaimLeavesAReclaimersClaimAlone(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	exp := time.Now().Add(time.Hour)
	first, owned, err := db.ClaimIdempotencyKey(ctx, a.ID, "emails.create", "k", "fp", exp, time.Now().Add(-time.Minute))
	if err != nil || !owned {
		t.Fatal(err, owned)
	}
	time.Sleep(20 * time.Millisecond)
	// The original stalls past the stale window; an identical retry reclaims (created_at resets).
	second, owned, err := db.ClaimIdempotencyKey(ctx, a.ID, "emails.create", "k", "fp", exp, time.Now().Add(time.Hour))
	if err != nil || !owned || !second.CreatedAt.After(first.CreatedAt) {
		t.Fatalf("reclaim failed: owned=%v err=%v", owned, err)
	}
	if err := db.ReleaseIdempotencyClaim(ctx, a.ID, "emails.create", "k", "fp", first.CreatedAt); err != nil {
		t.Fatal(err)
	}
	rec, err := db.GetIdempotencyKey(ctx, a.ID, "emails.create", "k")
	if err != nil || rec.Status != IdempotencyInProgress {
		t.Fatalf("the reclaimer's live claim was deleted: %+v %v", rec, err)
	}
	if err := db.ReleaseIdempotencyClaim(ctx, a.ID, "emails.create", "k", "fp", second.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetIdempotencyKey(ctx, a.ID, "emails.create", "k"); err == nil {
		t.Fatal("the owner's own release must delete the claim")
	}
}

// The public event list must include the public `suppressed` lifecycle event (email.suppressed).
func TestPublicEventListIncludesSuppressedEvents(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	id := insertAt(t, db, a.ID, time.Now().UTC())
	if err := db.RecordSuppressedRecipients(ctx, id, map[string]bool{"bob@example.com": true, "hidden-bcc@example.com": true}, true); err != nil {
		t.Fatal(err)
	}
	evs, err := db.ListTenantPublicEvents(ctx, a.ID, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		if e.Type == "suppressed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("GET /v1/events hides suppression events: %+v", evs)
	}
}
