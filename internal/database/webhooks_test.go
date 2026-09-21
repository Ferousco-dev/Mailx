package database

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func insertTestWebhook(t *testing.T, db *DB, tenantID string, events ...string) WebhookSubscription {
	t.Helper()
	row, err := db.InsertWebhookSubscription(context.Background(), NewWebhookSubscription{
		TenantID: tenantID, URL: "https://example.com/hook", SecretCiphertext: []byte("ciphertext"),
		SecretNonce: []byte("123456789012"), EventTypes: events,
	})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestWebhookFanOutIsDurableFilteredAndZeroSubscriptionSafe(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	a := insertTestWebhook(t, db, tenant.ID, "email.queued")
	b := insertTestWebhook(t, db, tenant.ID, "email.queued", "email.failed")
	c := insertTestWebhook(t, db, tenant.ID, "email.delivered")
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.FanOutWebhookEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 1 || result.Deliveries != 2 {
		t.Fatalf("fan-out = %+v", result)
	}
	for _, subscriptionID := range []string{a.ID, b.ID} {
		rows, err := db.ListWebhookDeliveries(ctx, tenant.ID, subscriptionID, 10)
		if err != nil || len(rows) != 1 || rows[0].EventID == "" {
			t.Fatalf("subscription %s rows=%+v err=%v", subscriptionID, rows, err)
		}
	}
	if rows, _ := db.ListWebhookDeliveries(ctx, tenant.ID, c.ID, 10); len(rows) != 0 {
		t.Fatalf("non-subscriber received queued event: %+v", rows)
	}
	if again, err := db.FanOutWebhookEvents(ctx, 100); err != nil || again.Events != 0 {
		t.Fatalf("fan-out not idempotent: %+v %v", again, err)
	}
	events, _ := db.ListMessageEvents(ctx, msg.ID)
	if len(events) != 1 {
		t.Fatal("source event was changed or removed")
	}

	zero := newTestTenant(t, db)
	zeroMsg, _ := db.InsertMessage(ctx, sampleNewMessage(t, zero.ID))
	if _, err := db.FanOutWebhookEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var fanned *time.Time
	if err := db.pool.QueryRow(ctx, `SELECT fanned_out_at FROM events WHERE message_id=$1`, zeroMsg.ID).Scan(&fanned); err != nil || fanned == nil {
		t.Fatalf("zero-subscription event was not durably processed: %v %v", fanned, err)
	}
}

func TestWebhookClaimLeaseFencingAndAttemptHistory(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	insertTestWebhook(t, db, tenant.ID, "email.queued")
	msg, _ := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if _, err := db.FanOutWebhookEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	first, err := db.ClaimWebhookDelivery(ctx, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if first.Event.MessageID != msg.ID || first.AttemptCount != 1 {
		t.Fatalf("claim = %+v", first)
	}
	if _, err := db.ClaimWebhookDelivery(ctx, now.Add(30*time.Second), time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("active lease was concurrently claimed: %v", err)
	}
	second, err := db.ClaimWebhookDelivery(ctx, now.Add(2*time.Minute), time.Minute)
	if err != nil || second.ID != first.ID || second.Event.ID != first.Event.ID || second.AttemptCount != 2 {
		t.Fatalf("lease reclaim = %+v %v", second, err)
	}
	if err := db.CompleteWebhookAttempt(ctx, WebhookAttemptResult{
		DeliveryID: first.ID, LeaseToken: first.LeaseToken, AttemptNumber: 1,
		Status: "succeeded", CompletedAt: now,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale token overwrote new claim: %v", err)
	}
	code := 200
	if err := db.CompleteWebhookAttempt(ctx, WebhookAttemptResult{
		DeliveryID: second.ID, LeaseToken: second.LeaseToken, AttemptNumber: 2,
		Status: "succeeded", CompletedAt: now.Add(2 * time.Minute), ResponseCode: &code,
	}); err != nil {
		t.Fatal(err)
	}
	var attempts, inProgress, retrying, succeeded int
	if err := db.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE status='in_progress'),
		       count(*) FILTER (WHERE status='retrying'),
		       count(*) FILTER (WHERE status='succeeded')
		FROM webhook_delivery_attempts WHERE delivery_id=$1`, first.ID,
	).Scan(&attempts, &inProgress, &retrying, &succeeded); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || inProgress != 0 || retrying != 1 || succeeded != 1 {
		t.Fatalf("attempt audit history = total:%d in_progress:%d retrying:%d succeeded:%d", attempts, inProgress, retrying, succeeded)
	}
}

func TestWebhookConcurrentClaimsDoNotNormallyDuplicate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	insertTestWebhook(t, db, tenant.ID, "email.queued")
	_, _ = db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	_, _ = db.FanOutWebhookEvents(ctx, 100)

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := db.ClaimWebhookDelivery(ctx, time.Now().Add(time.Second), time.Minute)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	claimed, notFound := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			claimed++
		case errors.Is(err, ErrNotFound):
			notFound++
		default:
			t.Fatal(err)
		}
	}
	if claimed != 1 || notFound != 1 {
		t.Fatalf("concurrent claims: claimed=%d not_found=%d", claimed, notFound)
	}
}

func TestWebhookFanOutRollsBackOnDeliveryPersistenceFailure(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	sub := insertTestWebhook(t, db, tenant.ID, "email.queued")
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `
		CREATE FUNCTION reject_webhook_delivery() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected webhook persistence failure'; END $$;
		CREATE TRIGGER reject_webhook_delivery BEFORE INSERT ON webhook_deliveries
		FOR EACH ROW EXECUTE FUNCTION reject_webhook_delivery()`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.FanOutWebhookEvents(ctx, 10); err == nil {
		t.Fatal("fan-out unexpectedly survived injected delivery persistence failure")
	}
	var fanned *time.Time
	if err := db.pool.QueryRow(ctx, `SELECT fanned_out_at FROM events WHERE message_id=$1`, msg.ID).Scan(&fanned); err != nil {
		t.Fatal(err)
	}
	if fanned != nil {
		t.Fatal("event was marked fanned out without durable delivery obligations")
	}
	if _, err := db.pool.Exec(ctx, `DROP TRIGGER reject_webhook_delivery ON webhook_deliveries; DROP FUNCTION reject_webhook_delivery()`); err != nil {
		t.Fatal(err)
	}
	if result, err := db.FanOutWebhookEvents(ctx, 10); err != nil || result.Deliveries != 1 {
		t.Fatalf("fan-out did not recover: %+v %v", result, err)
	}
	if rows, err := db.ListWebhookDeliveries(ctx, tenant.ID, sub.ID, 10); err != nil || len(rows) != 1 {
		t.Fatalf("durable delivery after recovery: %+v %v", rows, err)
	}
}

func TestWebhookDeliveryAndPublicEventPagination(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	sub := insertTestWebhook(t, db, tenant.ID, "email.queued", "email.delivered")
	for range 3 {
		if _, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.FanOutWebhookEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	first, err := db.ListWebhookDeliveriesPage(ctx, tenant.ID, sub.ID, 2, nil)
	if err != nil || len(first) != 2 {
		t.Fatalf("first delivery page: %+v %v", first, err)
	}
	after := &WebhookDeliveryCursor{CreatedAt: first[1].CreatedAt, ID: first[1].ID}
	second, err := db.ListWebhookDeliveriesPage(ctx, tenant.ID, sub.ID, 2, after)
	if err != nil || len(second) != 1 || second[0].ID == first[0].ID || second[0].ID == first[1].ID {
		t.Fatalf("second delivery page: %+v %v", second, err)
	}

	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendEvent(ctx, tenant.ID, msg.ID, EventDeliveryAttempted, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendEvent(ctx, tenant.ID, msg.ID, EventDelivered, nil); err != nil {
		t.Fatal(err)
	}
	public, err := db.ListTenantPublicEvents(ctx, tenant.ID, 2, nil)
	if err != nil || len(public) != 2 {
		t.Fatalf("public event page: %+v %v", public, err)
	}
	for _, event := range public {
		if event.Type == EventDeliveryAttempted {
			t.Fatal("internal event leaked into public pagination")
		}
	}
}

func TestWebhookHotPathIndexesSupportQueries(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	sub := insertTestWebhook(t, db, tenant.ID, "email.queued")
	if _, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.FanOutWebhookEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		index string
		query string
		args  []any
	}{
		{"idx_events_pending_fanout", `EXPLAIN (FORMAT TEXT) SELECT id FROM events WHERE fanned_out_at IS NULL ORDER BY occurred_at,id LIMIT 100`, nil},
		{"idx_webhook_deliveries_pending", `EXPLAIN (FORMAT TEXT) SELECT id FROM webhook_deliveries WHERE status='pending' AND next_attempt_at <= $1 ORDER BY next_attempt_at,created_at,id LIMIT 1`, []any{time.Now().Add(time.Hour)}},
		{"idx_webhook_deliveries_subscription_created", `EXPLAIN (FORMAT TEXT) SELECT id FROM webhook_deliveries WHERE tenant_id=$1 AND subscription_id=$2 ORDER BY created_at DESC,id DESC LIMIT 20`, []any{tenant.ID, sub.ID}},
	}
	for _, check := range checks {
		rows, err := tx.Query(ctx, check.query, check.args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line)
		}
		rows.Close()
		if !strings.Contains(plan.String(), check.index) {
			t.Fatalf("query plan does not use %s:\n%s", check.index, plan.String())
		}
	}
}

func TestWebhookRetryAndDisableAreDurable(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	sub := insertTestWebhook(t, db, tenant.ID, "email.queued")
	_, _ = db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID))
	_, _ = db.FanOutWebhookEvents(ctx, 100)
	now := time.Now().UTC().Add(time.Second)
	claim, err := db.ClaimWebhookDelivery(ctx, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	next := now.Add(30 * time.Minute)
	code := 503
	if err := db.CompleteWebhookAttempt(ctx, WebhookAttemptResult{
		DeliveryID: claim.ID, LeaseToken: claim.LeaseToken, AttemptNumber: 1,
		Status: "retrying", CompletedAt: now, ResponseCode: &code,
		ErrorCategory: "http_retryable", NextRetryAt: &next,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimWebhookDelivery(ctx, now.Add(time.Minute), time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retry became due early: %v", err)
	}
	if err := db.DisableWebhookSubscription(ctx, tenant.ID, sub.ID); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.ListWebhookDeliveries(ctx, tenant.ID, sub.ID, 10)
	if len(rows) != 1 || rows[0].Status != "cancelled" {
		t.Fatalf("disable did not retain/cancel history: %+v", rows)
	}
}

func TestWebhookSubscriptionTenantIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	b := newTestTenant(t, db)
	sub := insertTestWebhook(t, db, a.ID, "email.failed")
	if _, err := db.GetWebhookSubscription(ctx, b.ID, sub.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant read = %v", err)
	}
	if _, err := db.RotateWebhookSecret(ctx, b.ID, sub.ID, []byte("x"), []byte("123456789012")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant rotate = %v", err)
	}
	if err := db.DisableWebhookSubscription(ctx, b.ID, sub.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant delete = %v", err)
	}
}

func TestWebhookOneActiveClaimPerTenantUnderConcurrency(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	insertTestWebhook(t, db, tenant.ID, "email.queued")
	for range 6 {
		if _, err := db.InsertMessage(ctx, sampleNewMessage(t, tenant.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := db.FanOutWebhookEvents(ctx, 100); err != nil || res.Deliveries != 6 {
		t.Fatalf("fan-out: %+v %v", res, err)
	}
	// A second tenant's work must still be claimable while the first is busy.
	other := newTestTenant(t, db)
	insertTestWebhook(t, db, other.ID, "email.queued")
	if _, err := db.InsertMessage(ctx, sampleNewMessage(t, other.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.FanOutWebhookEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	type result struct {
		tenant string
		err    error
	}
	results := make(chan result, 12)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claim, err := db.ClaimWebhookDelivery(ctx, time.Now().Add(time.Second), time.Minute)
			results <- result{claim.TenantID, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	perTenant := map[string]int{}
	for r := range results {
		switch {
		case r.err == nil:
			perTenant[r.tenant]++
		case errors.Is(r.err, ErrNotFound):
		default:
			t.Fatal(r.err)
		}
	}
	for tenantID, n := range perTenant {
		if n != 1 {
			t.Fatalf("tenant %s has %d simultaneously active claims, want 1", tenantID, n)
		}
	}
	if len(perTenant) != 2 {
		t.Fatalf("both tenants should each hold one claim, got %v", perTenant)
	}
}
