package database

import (
	"context"
	"strings"
	"testing"
)

// TestQueryPlansUseExpectedIndexes seeds enough rows that the planner
// prefers an index scan over a sequential scan for MailX's real listing
// queries, then inspects EXPLAIN output. A tiny table would correctly
// prefer Seq Scan — that is not a bug, so this test seeds a few hundred
// rows specifically to make the index scan meaningfully cheaper.
func TestQueryPlansUseExpectedIndexes(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	other, err := db.CreateTenant(ctx, "other-tenant")
	if err != nil {
		t.Fatal(err)
	}

	const seedCount = 500
	for i := 0; i < seedCount; i++ {
		if _, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID)); err != nil {
			t.Fatal(err)
		}
	}
	// Seed a second tenant too, so the planner has a real reason to prefer
	// the composite index over a full scan + filter.
	for i := 0; i < seedCount; i++ {
		if _, err := db.InsertMessage(ctx, sampleNewMessage(t, other.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.pool.Exec(ctx, `ANALYZE messages`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `ANALYZE recipients`); err != nil {
		t.Fatal(err)
	}

	t.Run("list messages by tenant ordered by created_at", func(t *testing.T) {
		// No status filter: idx_messages_tenant_created (added in
		// migration 000002 after this exact EXPLAIN check first revealed
		// idx_messages_tenant_status_created cannot serve this shape — see
		// that migration's comment for the full explanation) is expected.
		plan := explain(t, db, `
			SELECT id FROM messages
			WHERE tenant_id = $1
			ORDER BY created_at DESC
			LIMIT 50`, tenant.ID)
		t.Logf("query plan:\n%s", plan)
		if !strings.Contains(plan, "idx_messages_tenant_created") {
			t.Fatalf("expected planner to use idx_messages_tenant_created, got:\n%s", plan)
		}
	})

	// Give status genuine selectivity: mark half of tenant's messages
	// delivered, so a status='queued' filter is not a no-op predicate
	// matching 100% of rows (which would legitimately make the planner
	// ignore the composite index as pointless).
	rows, err := db.ListMessages(ctx, tenant.ID, nil, seedCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range rows {
		if i%2 == 0 {
			if err := db.UpdateMessageStatus(ctx, m.ID, StatusDelivered, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.pool.Exec(ctx, `ANALYZE messages`); err != nil {
		t.Fatal(err)
	}

	t.Run("list messages by tenant and status ordered by created_at", func(t *testing.T) {
		plan := explain(t, db, `
			SELECT id FROM messages
			WHERE tenant_id = $1 AND status = 'queued'
			ORDER BY created_at DESC
			LIMIT 50`, tenant.ID)
		t.Logf("query plan:\n%s", plan)
		if !strings.Contains(plan, "idx_messages_tenant_status_created") {
			t.Fatalf("expected planner to use idx_messages_tenant_status_created, got:\n%s", plan)
		}
	})

	t.Run("get message by tenant and id", func(t *testing.T) {
		msgs, err := db.ListMessages(ctx, tenant.ID, nil, 1, nil)
		if err != nil || len(msgs) == 0 {
			t.Fatal(err)
		}
		plan := explain(t, db, `
			SELECT id FROM messages WHERE tenant_id = $1 AND id = $2`, tenant.ID, msgs[0].ID)
		t.Logf("query plan:\n%s", plan)
		// Either the composite unique index or the PK index is an
		// acceptable, correct plan here — both are O(1)-ish lookups; the
		// important negative assertion is that it is NOT a sequential
		// scan of a 1000-row table.
		if strings.Contains(plan, "Seq Scan") {
			t.Fatalf("expected an index lookup, got a sequential scan:\n%s", plan)
		}
	})
}

// TestRecipientsLookupUsesForeignKeyIndex proves idx_recipients_message_id
// is actually used — Postgres does NOT automatically index foreign key
// columns, only the referenced side, so without this index a lookup (and
// every ON DELETE CASCADE) would sequentially scan recipients.
func TestRecipientsLookupUsesForeignKeyIndex(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)

	var targetID string
	for i := 0; i < 500; i++ {
		msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
		if err != nil {
			t.Fatal(err)
		}
		if i == 250 {
			targetID = msg.ID
		}
	}
	if _, err := db.pool.Exec(ctx, `ANALYZE recipients`); err != nil {
		t.Fatal(err)
	}

	plan := explain(t, db, `SELECT id FROM recipients WHERE message_id = $1`, targetID)
	t.Logf("query plan:\n%s", plan)
	if strings.Contains(plan, "Seq Scan") {
		t.Fatalf("expected idx_recipients_message_id to be used, got a sequential scan:\n%s", plan)
	}
}

func TestDeliveryOutcomeEventLookupUsesUniqueIndex(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	var targetID string
	for i := 0; i < 300; i++ {
		msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
		if err != nil {
			t.Fatal(err)
		}
		attempt := sampleAttempt(msg.ID, 1, DecisionRetry)
		if err := db.PersistDeliveryOutcome(ctx, attempt, false, nil); err != nil {
			t.Fatal(err)
		}
		if i == 150 {
			targetID = msg.ID
		}
	}
	if _, err := db.pool.Exec(ctx, `ANALYZE events`); err != nil {
		t.Fatal(err)
	}
	plan := explainAnalyze(t, db, `
		SELECT id FROM events
		WHERE message_id = $1 AND delivery_attempt_number = 1`, targetID)
	t.Logf("query plan:\n%s", plan)
	if !strings.Contains(plan, "uq_events_message_delivery_attempt") {
		t.Fatalf("expected delivery-outcome uniqueness index, got:\n%s", plan)
	}
}

// explain runs EXPLAIN (without ANALYZE, to keep this deterministic and
// side-effect-free for read-only planning inspection) and returns the plan
// text.
func explain(t *testing.T, db *DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.pool.Query(context.Background(), "EXPLAIN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func explainAnalyze(t *testing.T, db *DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.pool.Query(context.Background(), "EXPLAIN (ANALYZE, BUFFERS) "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
