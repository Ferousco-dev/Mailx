package database

import (
	"context"
	"strings"
	"testing"
	"time"
)

// insertEvent writes one events row directly at occurredAt — the analytics
// tests need exact control over the fact's timestamp (for [from,to) and
// bucket-boundary tests) that going through the full delivery/feedback
// pipelines would not give cheaply.
func insertEvent(t *testing.T, db *DB, tenantID, messageID string, eventType EventType, occurredAt time.Time) {
	t.Helper()
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(context.Background(),
		`INSERT INTO events (id, tenant_id, message_id, event_type, occurred_at) VALUES ($1,$2,$3,$4,$5)`,
		id, tenantID, messageID, string(eventType), occurredAt,
	); err != nil {
		t.Fatal(err)
	}
}

func TestAnalyticsOverviewCountsByEventTypeWithinRange(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	insertEvent(t, db, tn.ID, msg.ID, EventQueued, base)
	insertEvent(t, db, tn.ID, msg.ID, EventDelivered, base.Add(time.Minute))
	// A later async bounce must NOT erase the earlier Queued/Delivered facts.
	insertEvent(t, db, tn.ID, msg.ID, EventBounced, base.Add(time.Hour))
	insertEvent(t, db, tn.ID, msg.ID, EventComplained, base.Add(2*time.Hour))

	out, err := db.AnalyticsOverview(ctx, tn.ID, base.Add(-time.Minute), base.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if out.Counts.Queued != 1 || out.Counts.Delivered != 1 || out.Counts.Bounced != 1 || out.Counts.Complained != 1 {
		t.Fatalf("earlier acceptance facts must survive a later bounce/complaint: %+v", out.Counts)
	}
}

// [from, to) must be half-open: a fact exactly AT `from` counts, a fact
// exactly AT `to` does not.
func TestAnalyticsOverviewHalfOpenBoundary(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	insertEvent(t, db, tn.ID, msg.ID, EventQueued, from)                     // at from: included
	insertEvent(t, db, tn.ID, msg.ID, EventDelivered, to)                    // at to: excluded
	insertEvent(t, db, tn.ID, msg.ID, EventFailed, to.Add(-time.Nanosecond)) // just before to: included

	out, err := db.AnalyticsOverview(ctx, tn.ID, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if out.Counts.Queued != 1 {
		t.Fatalf("event at exactly `from` must be included: %+v", out.Counts)
	}
	if out.Counts.Delivered != 0 {
		t.Fatalf("event at exactly `to` must be EXCLUDED (half-open range): %+v", out.Counts)
	}
	if out.Counts.Failed != 1 {
		t.Fatalf("event just before `to` must be included: %+v", out.Counts)
	}
}

func TestAnalyticsOverviewTenantIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a, b := newTestTenant(t, db), newTestTenant(t, db)
	msgA, _ := db.InsertMessage(ctx, sampleNewMessage(t, a.ID))
	msgB, _ := db.InsertMessage(ctx, sampleNewMessage(t, b.ID))
	now := time.Now().UTC()
	insertEvent(t, db, a.ID, msgA.ID, EventDelivered, now)
	insertEvent(t, db, b.ID, msgB.ID, EventDelivered, now)
	insertEvent(t, db, b.ID, msgB.ID, EventDelivered, now)

	out, err := db.AnalyticsOverview(ctx, a.ID, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if out.Counts.Delivered != 1 {
		t.Fatalf("tenant A must not see tenant B's events: %+v", out.Counts)
	}
}

func TestAnalyticsOverviewCurrentlySuppressedIsCurrentStateNotHistorical(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	if _, _, err := db.CreateSuppression(ctx, NewSuppression{TenantID: tn.ID, Email: "a@example.com", Reason: "manual", Source: "api"}); err != nil {
		t.Fatal(err)
	}
	// A range that excludes "now" entirely — CurrentlySuppressed must still
	// reflect current state, since it is NOT a time-bucketed historical fact.
	past := time.Now().UTC().Add(-48 * time.Hour)
	out, err := db.AnalyticsOverview(ctx, tn.ID, past, past.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if out.CurrentlySuppressed != 1 {
		t.Fatalf("CurrentlySuppressed = %d, want 1 (current state, independent of the requested range)", out.CurrentlySuppressed)
	}
}

func TestAnalyticsOverviewRejectsInvalidRange(t *testing.T) {
	db := newTestDB(t)
	tn := newTestTenant(t, db)
	now := time.Now().UTC()
	if _, err := db.AnalyticsOverview(context.Background(), tn.ID, now, now); err == nil {
		t.Fatal("expected an error for to == from")
	}
	if _, err := db.AnalyticsOverview(context.Background(), tn.ID, now, now.Add(-time.Hour)); err == nil {
		t.Fatal("expected an error for to < from")
	}
}

func TestAnalyticsTimeseriesBucketsByIntervalAndOmitsEmptyBuckets(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	msg, _ := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	day1 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	day3 := day1.Add(48 * time.Hour) // day2 deliberately has NO events
	insertEvent(t, db, tn.ID, msg.ID, EventQueued, day1)
	insertEvent(t, db, tn.ID, msg.ID, EventQueued, day3)

	buckets, err := db.AnalyticsTimeseries(ctx, tn.ID, day1.Add(-time.Hour), day3.Add(24*time.Hour), "day")
	if err != nil {
		t.Fatal(err)
	}
	// Sparse: only 2 buckets (day1, day3), day2 absent entirely — not zero-filled.
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2 (sparse, empty day omitted): %+v", len(buckets), buckets)
	}
	if !buckets[0].Timestamp.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("bucket[0] = %v, want day1 truncated to UTC midnight", buckets[0].Timestamp)
	}
	if buckets[0].Counts.Queued != 1 || buckets[1].Counts.Queued != 1 {
		t.Fatalf("%+v", buckets)
	}
	// Ascending order.
	if !buckets[0].Timestamp.Before(buckets[1].Timestamp) {
		t.Fatalf("buckets not ascending: %+v", buckets)
	}
}

func TestAnalyticsTimeseriesRejectsUnsupportedInterval(t *testing.T) {
	db := newTestDB(t)
	tn := newTestTenant(t, db)
	now := time.Now().UTC()
	if _, err := db.AnalyticsTimeseries(context.Background(), tn.ID, now.Add(-time.Hour), now, "week"); err == nil {
		t.Fatal("expected an error for an unwhitelisted interval")
	}
}

// A huge range with a tiny interval must be rejected, not silently return
// tens of thousands of points.
func TestAnalyticsTimeseriesRejectsTooManyBuckets(t *testing.T) {
	db := newTestDB(t)
	tn := newTestTenant(t, db)
	now := time.Now().UTC()
	_, err := db.AnalyticsTimeseries(context.Background(), tn.ID, now.Add(-365*24*time.Hour), now, "hour")
	if err == nil {
		t.Fatal("expected ErrTooManyBuckets for a year of hourly buckets")
	}
}

func TestBroadcastAnalyticsUsesDurableSnapshotNotLiveAudience(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn, aud, tmpl := setupAudienceFixture(t, db)
	c1 := mustContact(t, db, tn.ID, "one@example.com")
	c2 := mustContact(t, db, tn.ID, "two@example.com")
	db.AddAudienceMember(ctx, tn.ID, aud.ID, c1.ID)
	db.AddAudienceMember(ctx, tn.ID, aud.ID, c2.ID)
	b := createBroadcast(t, db, tn, aud, tmpl)
	if _, _, err := db.SnapshotBroadcastBatch(ctx, b, 10); err != nil {
		t.Fatal(err)
	}
	recipients, err := db.ListBroadcastRecipients(ctx, tn.ID, b.ID, 10, nil)
	if err != nil || len(recipients) != 2 {
		t.Fatalf("%+v %v", recipients, err)
	}
	// One materialized (with a delivered event), one still pending.
	// broadcast_recipients.id doubles as messages.id once materialized (see
	// migration 000019's doc) — a real message row must exist for events'
	// FK, exactly as real materialization creates one via InsertMessage.
	mm := sampleNewMessage(t, tn.ID)
	mm.ID = recipients[0].ID
	if _, err := db.InsertMessage(ctx, mm); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkBroadcastRecipientMaterialized(ctx, recipients[0].ID, recipients[0].ID); err != nil {
		t.Fatal(err)
	}
	insertEvent(t, db, tn.ID, recipients[0].ID, EventDelivered, time.Now().UTC())

	// A member REMOVED from the live Audience after the snapshot must NOT
	// change broadcast analytics — the durable snapshot is authoritative.
	if err := db.RemoveAudienceMember(ctx, tn.ID, aud.ID, c2.ID); err != nil {
		t.Fatal(err)
	}

	a, err := db.BroadcastAnalytics(ctx, tn.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Intended != 2 {
		t.Fatalf("Intended = %d, want 2 (durable snapshot, unaffected by the later Audience removal)", a.Intended)
	}
	if a.Materialized != 1 || a.Pending != 1 {
		t.Fatalf("%+v", a)
	}
	if a.Delivered != 1 {
		t.Fatalf("Delivered = %d, want 1", a.Delivered)
	}
}

func TestBroadcastAnalyticsCrossTenantIsNotFound(t *testing.T) {
	db := newTestDB(t)
	_, _, b := setupBroadcastFixture(t, db)
	other := newTestTenant(t, db)
	if _, err := db.BroadcastAnalytics(context.Background(), other.ID, b.ID); err == nil {
		t.Fatal("expected an error for a cross-tenant broadcast id")
	}
}

// TestAnalyticsHotQueriesUseIndexes is EXPLAIN evidence at synthetic scale
// that both the overview and timeseries queries use idx_events_tenant_occurred
// rather than sequentially scanning events.
func TestAnalyticsHotQueriesUseIndexes(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	msg, _ := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	// Spread 50k rows across 90 days so a 1-day query window is genuinely
	// SELECTIVE (~1/90th of the table) — querying nearly 100% of a table
	// correctly prefers a sequential scan (that would be the WRONG index
	// to reach for), so the plan must be judged at realistic selectivity,
	// not an artificially wide query range.
	base := time.Now().UTC().Add(-90 * 24 * time.Hour)
	const n = 50000
	// 'suppressed' excluded from the cycle: uq_events_message_suppressed
	// allows at most one per message_id, and this scale test intentionally
	// reuses ONE message_id for every synthetic row (only occurred_at/
	// event_type vary) to stress the index without needing 50k real
	// message rows — a real workload has many distinct message_ids instead.
	if _, err := db.pool.Exec(ctx, `
		INSERT INTO events (id, tenant_id, message_id, event_type, occurred_at)
		SELECT 'ev'||g, $1, $2,
			(ARRAY['queued','delivered','deferred','bounced','failed','complained'])[1 + (g % 6)],
			$3::timestamptz + (g * 155 || ' seconds')::interval
		FROM generate_series(1, $4) g`,
		tn.ID, msg.ID, base, n); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `ANALYZE events`); err != nil {
		t.Fatal(err)
	}

	// Query only the most recent 1 day of the 90-day spread.
	to := time.Now().UTC()
	from := to.Add(-24 * time.Hour)
	overviewPlan := explainAnalyze(t, db, `SELECT`+eventTypeCountColumns+`
		FROM events WHERE tenant_id = $1 AND occurred_at >= $2 AND occurred_at < $3`, tn.ID, from, to)
	if strings.Contains(overviewPlan, "Seq Scan on events") {
		t.Fatalf("analytics overview sequentially scans events at %d rows:\n%s", n, overviewPlan)
	}

	tsPlan := explainAnalyze(t, db, `
		SELECT date_trunc($4, occurred_at) AS bucket,`+eventTypeCountColumns+`
		FROM events WHERE tenant_id = $1 AND occurred_at >= $2 AND occurred_at < $3
		GROUP BY bucket ORDER BY bucket LIMIT $5`, tn.ID, from, to, "day", AnalyticsMaxBuckets+1)
	if strings.Contains(tsPlan, "Seq Scan on events") {
		t.Fatalf("analytics timeseries sequentially scans events at %d rows:\n%s", n, tsPlan)
	}
	t.Logf("overview plan:\n%s\ntimeseries plan:\n%s", overviewPlan, tsPlan)
}
