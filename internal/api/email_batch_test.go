package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

func TestBatchSendAllAccepted(t *testing.T) {
	mux, _, _ := setupSendMux(t)
	rec := doJSON(t, mux, "POST", "/v1/emails/batch", map[string]any{
		"emails": []map[string]any{
			{"from": "a@example.com", "to": []string{"bob@example.com"}, "subject": "one", "text": "1"},
			{"from": "a@example.com", "to": []string{"carol@example.com"}, "subject": "two", "text": "2"},
		},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got batchSendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Accepted != 2 || got.Rejected != 0 || len(got.Data) != 2 {
		t.Fatalf("unexpected response: %+v", got)
	}
	for i, item := range got.Data {
		if item.Index != i || item.Email == nil || item.Email.ID == "" || item.Error != nil {
			t.Fatalf("item %d not accepted: %+v", i, item)
		}
	}
	if got.Data[0].Email.ID == got.Data[1].Email.ID {
		t.Fatal("batch items must produce distinct message ids")
	}
}

func TestBatchSendPartialFailureIndependentItems(t *testing.T) {
	mux, _, _ := setupSendMux(t)
	rec := doJSON(t, mux, "POST", "/v1/emails/batch", map[string]any{
		"emails": []map[string]any{
			{"from": "a@example.com", "to": []string{"bob@example.com"}, "subject": "ok-1", "text": "1"},
			{"from": "a@example.com", "to": []string{}, "subject": "bad-missing-recipient", "text": "x"}, // invalid: no recipients
			{"from": "a@example.com", "to": []string{"dave@example.com"}, "subject": "ok-2", "text": "3"},
			{"from": "alice@evil.com", "to": []string{"eve@example.com"}, "subject": "bad-domain", "text": "x"},
		},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got batchSendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Accepted != 2 || got.Rejected != 2 || len(got.Data) != 4 {
		t.Fatalf("unexpected response: %+v", got)
	}
	// order preserved, each item's outcome independently correct
	if got.Data[0].Email == nil || got.Data[0].Error != nil {
		t.Fatalf("item 0 should be accepted: %+v", got.Data[0])
	}
	if got.Data[1].Email != nil || got.Data[1].Error == nil || got.Data[1].Error.Code != "missing_recipient" {
		t.Fatalf("item 1 should be rejected (missing_recipient): %+v", got.Data[1])
	}
	if got.Data[2].Email == nil || got.Data[2].Error != nil {
		t.Fatalf("item 2 should be accepted: %+v", got.Data[2])
	}
	if got.Data[3].Email != nil || got.Data[3].Error == nil || got.Data[3].Error.Code != "from_domain_not_authorized" {
		t.Fatalf("item 3 should be rejected (from_domain_not_authorized): index=%d email=%v error=%+v", got.Data[3].Index, got.Data[3].Email, got.Data[3].Error)
	}
}

func TestBatchSendEmptyRejected(t *testing.T) {
	mux, _, _ := setupSendMux(t)
	rec := doJSON(t, mux, "POST", "/v1/emails/batch", map[string]any{"emails": []map[string]any{}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBatchSendOversizedRejectedBeforeAnyItemProcessed(t *testing.T) {
	mux, db, tenantID := setupSendMux(t)
	items := make([]map[string]any, MaxBatchSize+1)
	for i := range items {
		items[i] = map[string]any{"from": "a@example.com", "to": []string{"bob@example.com"}, "subject": "x", "text": "x"}
	}
	rec := doJSON(t, mux, "POST", "/v1/emails/batch", map[string]any{"emails": items})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	list, err := db.ListMessages(context.Background(), tenantID, nil, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("an oversized batch must not process ANY item, found %d messages", len(list))
	}
}

func TestBatchSendMalformedJSONRejected(t *testing.T) {
	mux, _, _ := setupSendMux(t)
	rec := doJSON(t, mux, "POST", "/v1/emails/batch", "not an object")
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBatchSendUnknownFieldRejected(t *testing.T) {
	mux, _, _ := setupSendMux(t)
	rec := doJSON(t, mux, "POST", "/v1/emails/batch", map[string]any{
		"emails":       []map[string]any{{"from": "a@example.com", "to": []string{"bob@example.com"}, "text": "x"}},
		"unknownfield": "x",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBatchSendItemIdempotencyKeyReplay(t *testing.T) {
	mux, _, _ := setupSendMux(t)
	body := map[string]any{
		"emails": []map[string]any{
			{"from": "a@example.com", "to": []string{"bob@example.com"}, "subject": "s", "text": "t", "idempotency_key": "batch-item-1"},
		},
	}
	rec1 := doJSON(t, mux, "POST", "/v1/emails/batch", body)
	var got1 batchSendResponse
	if err := json.Unmarshal(rec1.Body.Bytes(), &got1); err != nil {
		t.Fatal(err)
	}
	if got1.Accepted != 1 {
		t.Fatalf("first request: %+v", got1)
	}
	rec2 := doJSON(t, mux, "POST", "/v1/emails/batch", body)
	var got2 batchSendResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &got2); err != nil {
		t.Fatal(err)
	}
	if got2.Accepted != 1 || got2.Data[0].Email.ID != got1.Data[0].Email.ID {
		t.Fatalf("replay must return the SAME message id, got %+v vs %+v", got1, got2)
	}
}

func TestBatchSendItemIdempotencyKeyConflict(t *testing.T) {
	mux, _, _ := setupSendMux(t)
	rec1 := doJSON(t, mux, "POST", "/v1/emails/batch", map[string]any{
		"emails": []map[string]any{
			{"from": "a@example.com", "to": []string{"bob@example.com"}, "subject": "s", "text": "t", "idempotency_key": "dup-key"},
		},
	})
	var got1 batchSendResponse
	_ = json.Unmarshal(rec1.Body.Bytes(), &got1)
	if got1.Accepted != 1 {
		t.Fatalf("first request: %+v", got1)
	}
	rec2 := doJSON(t, mux, "POST", "/v1/emails/batch", map[string]any{
		"emails": []map[string]any{
			{"from": "a@example.com", "to": []string{"carol@example.com"}, "subject": "different", "text": "t", "idempotency_key": "dup-key"},
		},
	})
	var got2 batchSendResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &got2); err != nil {
		t.Fatal(err)
	}
	if got2.Rejected != 1 || got2.Data[0].Error == nil || got2.Data[0].Error.Code != "idempotency_key_conflict" {
		t.Fatalf("expected idempotency_key_conflict for a reused key with a different payload: %+v", got2)
	}
}

func TestBatchSendInvalidIdempotencyKeyRejectsWholeBatchBeforeAnyItem(t *testing.T) {
	mux, db, tenantID := setupSendMux(t)
	rec := doJSON(t, mux, "POST", "/v1/emails/batch", map[string]any{
		"emails": []map[string]any{
			{"from": "a@example.com", "to": []string{"bob@example.com"}, "subject": "s", "text": "t"},
			{"from": "a@example.com", "to": []string{"carol@example.com"}, "subject": "s", "text": "t", "idempotency_key": "bad\r\nkey"},
		},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	list, err := db.ListMessages(context.Background(), tenantID, nil, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("a batch with any malformed idempotency key must process NO item, found %d messages", len(list))
	}
}

// A batch cannot buy a tenant more throughput than the same N individual
// POST /v1/emails calls would have gotten: each item independently runs
// admitSend, so the tenant queue cap still bites mid-batch.
func TestBatchSendAbuseControlsAppliedPerItemIndependently(t *testing.T) {
	p := testPolicy()
	p.TenantMaxQueuedMessages = 2
	rig := newAbuseRig(t, p)
	_, h := rig.tenant("acme")
	from := h.(acctHandler).from
	rec := doJSON(t, h, "POST", "/v1/emails/batch", map[string]any{
		"emails": []map[string]any{
			{"from": from, "to": []string{"a0@dest.test"}, "subject": "s", "text": "t"},
			{"from": from, "to": []string{"a1@dest.test"}, "subject": "s", "text": "t"},
			{"from": from, "to": []string{"a2@dest.test"}, "subject": "s", "text": "t"},
		},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got batchSendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Accepted != 2 || got.Rejected != 1 {
		t.Fatalf("expected the tenant queue cap (2) to bite on the 3rd item, got accepted=%d rejected=%d: %+v", got.Accepted, got.Rejected, got)
	}
	if got.Data[2].Error == nil || got.Data[2].Error.Code != "tenant_queue_full" {
		t.Fatalf("expected item 2 to fail with tenant_queue_full, got %+v", got.Data[2])
	}
}

// PR review finding: a 100-item batch must not spend a single request-rate
// token to perform up to 100x the CPU/DB work of a normal send. The
// remaining (N-1) tokens must be charged against the same request buckets
// requestLimitMiddleware already uses, BEFORE any expensive per-item work
// runs, so a batch too large for the burst is refused wholesale.
func TestBatchSendChargesRequestRateLimitProportionalToSize(t *testing.T) {
	p := testPolicy()
	p.TenantRequestRate, p.TenantRequestBurst = 100, 5 // burst=5: a 3-item batch (charging 1 middleware + 2 extra) fits; two of them do not
	rig := newAbuseRig(t, p)
	_, h := rig.tenant("acme")
	from := h.(acctHandler).from

	batch := func(n int) map[string]any {
		items := make([]map[string]any, n)
		for i := range items {
			items[i] = map[string]any{"from": from, "to": []string{fmt.Sprintf("r%d@dest.test", i)}, "subject": "s", "text": "t"}
		}
		return map[string]any{"emails": items}
	}

	// First 3-item batch: middleware charges 1, handler charges 2 more = 3 of burst 5. Fits.
	rec := doJSON(t, h, "POST", "/v1/emails/batch", batch(3))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first batch: got %d: %s", rec.Code, rec.Body.String())
	}
	var got batchSendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Accepted != 3 {
		t.Fatalf("first batch: expected all 3 accepted, got %+v", got)
	}

	// Second batch of 3 more (middleware 1 + handler 2 = 3, but only 2 tokens
	// remain in the burst of 5 after the first batch spent 3): must be
	// refused WHOLESALE (429), before any of its 3 items is processed.
	rec2 := doJSON(t, h, "POST", "/v1/emails/batch", batch(3))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second batch: expected 429 (rate limit), got %d: %s", rec2.Code, rec2.Body.String())
	}
}

// A batch of 1 must behave exactly as before this fix: no extra charge
// call at all (the existing single middleware charge is already correct
// for one item), so a tenant sending single-item batches sees no new
// rate-limit interaction.
func TestBatchSendSingleItemDoesNotDoubleCharge(t *testing.T) {
	p := testPolicy()
	p.TenantRequestRate, p.TenantRequestBurst = 100, 1 // burst=1: only room for the middleware's own charge
	rig := newAbuseRig(t, p)
	_, h := rig.tenant("acme")
	from := h.(acctHandler).from
	rec := doJSON(t, h, "POST", "/v1/emails/batch", map[string]any{
		"emails": []map[string]any{{"from": from, "to": []string{"r0@dest.test"}, "subject": "s", "text": "t"}},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

// PR review finding: with several items whose idempotency keys are already
// claimed in-progress elsewhere (each worth up to idempotencyPollTimeout
// of blocking), the batch must not silently hang past its processing
// budget — remaining items get an explicit timeout result instead.
func TestBatchSendRemainingItemsTimeOutRatherThanHang(t *testing.T) {
	db := newTestDB(t)
	tenant := newTestTenant(t, db)
	verifyTestDomain(t, db, tenant.ID, "example.com")
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eh := newEmailHandler(db, store)
	eh.batchBudget = 1 * time.Nanosecond // expires before the loop's first ctx.Err() check on item 2+
	authSvc := auth.NewService(db, nil)
	gen, _, err := authSvc.Create(context.Background(), tenant.ID, "k", []string{string(auth.ScopeEmailsSend)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := authInjector{next: newMux(eh, authSvc, func() error { return nil }), token: gen.Raw}
	tenantID := tenant.ID

	rec := doJSON(t, mux, "POST", "/v1/emails/batch", map[string]any{
		"emails": []map[string]any{
			{"from": "a@example.com", "to": []string{"bob@example.com"}, "subject": "s", "text": "t"},
			{"from": "a@example.com", "to": []string{"carol@example.com"}, "subject": "s", "text": "t"},
			{"from": "a@example.com", "to": []string{"dave@example.com"}, "subject": "s", "text": "t"},
		},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got batchSendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Data) != 3 {
		t.Fatalf("expected exactly one result per item regardless of the timeout, got %d", len(got.Data))
	}
	var timedOut int
	for _, item := range got.Data {
		if item.Error != nil && item.Error.Code == "batch_processing_timeout" {
			timedOut++
		}
	}
	if timedOut != 3 {
		t.Fatalf("with an already-expired budget every item must time out at the loop's pre-check (before acceptOne is even called), got %+v", got)
	}
	list, err := db.ListMessages(context.Background(), tenantID, nil, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("no item should have been processed at all, found %d messages", len(list))
	}
}

func TestBatchSendRequiresEmailsSendScope(t *testing.T) {
	mux, db, tenant, authSvc, _ := setupMuxNoAuth(t)
	verifyTestDomain(t, db, tenant.ID, "example.com")
	// A key without emails:send must be refused entirely (403), never
	// partially processed.
	gen, _, err := authSvc.Create(context.Background(), tenant.ID, "read-only", []string{string(auth.ScopeEmailsRead)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := authInjector{next: mux, token: gen.Raw}
	req := map[string]any{"emails": []map[string]any{
		{"from": "a@example.com", "to": []string{"bob@example.com"}, "subject": "s", "text": "t"},
	}}
	rec := doJSON(t, client, "POST", "/v1/emails/batch", req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}
