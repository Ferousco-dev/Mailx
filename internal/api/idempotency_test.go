package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/idempotency"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

// doJSONWithKey is doJSON plus an Idempotency-Key header, for the tests
// in this file specifically exercising v0.20 behavior.
func doJSONWithKey(t *testing.T, mux http.Handler, method, path, idemKey string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeEmail(t *testing.T, rec *httptest.ResponseRecorder) email {
	t.Helper()
	var e email
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("response is not a valid Email: %v (%s)", err, rec.Body.String())
	}
	return e
}

func samplePayload() map[string]any {
	return map[string]any{"from": "a@example.com", "to": []string{"b@example.com"}, "subject": "hi", "text": "hello"}
}

func TestIdempotencyNoKeyBehavesLikeBeforeV020(t *testing.T) {
	mux, _, _ := setupMux(t)
	first := doJSON(t, mux, "POST", "/v1/emails", samplePayload())
	second := doJSON(t, mux, "POST", "/v1/emails", samplePayload())
	if first.Code != http.StatusAccepted || second.Code != http.StatusAccepted {
		t.Fatalf("got %d and %d", first.Code, second.Code)
	}
	e1, e2 := decodeEmail(t, first), decodeEmail(t, second)
	if e1.ID == e2.ID {
		t.Fatal("without an Idempotency-Key, two POSTs must create two distinct emails")
	}
}

func TestIdempotencyIdenticalRetryReturnsSameEmail(t *testing.T) {
	mux, db, tenantID := setupMux(t)
	first := doJSONWithKey(t, mux, "POST", "/v1/emails", "key-abc", samplePayload())
	if first.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", first.Code, first.Body.String())
	}
	e1 := decodeEmail(t, first)

	second := doJSONWithKey(t, mux, "POST", "/v1/emails", "key-abc", samplePayload())
	if second.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", second.Code, second.Body.String())
	}
	e2 := decodeEmail(t, second)
	if e1.ID != e2.ID {
		t.Fatalf("expected the same email id on replay, got %s then %s", e1.ID, e2.ID)
	}
	if second.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("expected the replay to be marked via the Idempotency-Replayed header")
	}
	if first.Header().Get("Idempotency-Replayed") == "true" {
		t.Fatal("the FIRST response must not claim to be a replay")
	}

	rows, err := db.ListMessages(context.Background(), tenantID, nil, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range rows {
		if r.ID == e1.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one message row for the idempotent id, got %d", count)
	}
}

func TestIdempotencyDifferentKeyCreatesNewEmail(t *testing.T) {
	mux, _, _ := setupMux(t)
	first := doJSONWithKey(t, mux, "POST", "/v1/emails", "key-a", samplePayload())
	second := doJSONWithKey(t, mux, "POST", "/v1/emails", "key-b", samplePayload())
	e1, e2 := decodeEmail(t, first), decodeEmail(t, second)
	if e1.ID == e2.ID {
		t.Fatal("different idempotency keys must create different emails")
	}
}

func TestIdempotencySameKeyDifferentPayloadConflicts(t *testing.T) {
	mux, _, _ := setupMux(t)
	first := doJSONWithKey(t, mux, "POST", "/v1/emails", "key-x", samplePayload())
	if first.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", first.Code, first.Body.String())
	}

	changed := samplePayload()
	changed["subject"] = "a materially different subject"
	second := doJSONWithKey(t, mux, "POST", "/v1/emails", "key-x", changed)
	if second.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", second.Code, second.Body.String())
	}
	body := decodeError(t, second)
	if body.Error.Code != "idempotency_key_conflict" {
		t.Fatalf("unexpected error code: %+v", body)
	}
	if strings.Contains(second.Body.String(), "materially different subject") {
		t.Fatal("conflict response must never echo the original request content")
	}
}

func TestIdempotencyInvalidKeyRejected(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doJSONWithKey(t, mux, "POST", "/v1/emails", strings.Repeat("a", idempotency.MaxKeyLen+1), samplePayload())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestIdempotencyValidationFailureDoesNotConsumeKey(t *testing.T) {
	mux, _, _ := setupMux(t)
	bad := map[string]any{"from": "a@example.com", "to": []string{}, "text": "x"} // no recipients
	firstRec := doJSONWithKey(t, mux, "POST", "/v1/emails", "retry-key", bad)
	if firstRec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected the invalid request to fail validation, got %d: %s", firstRec.Code, firstRec.Body.String())
	}

	good := samplePayload()
	secondRec := doJSONWithKey(t, mux, "POST", "/v1/emails", "retry-key", good)
	if secondRec.Code != http.StatusAccepted {
		t.Fatalf("expected the corrected retry with the SAME key to succeed, got %d: %s", secondRec.Code, secondRec.Body.String())
	}
}

func TestIdempotencyMalformedJSONDoesNotConsumeKey(t *testing.T) {
	mux, _, _ := setupMux(t)
	req := httptest.NewRequest("POST", "/v1/emails", strings.NewReader(`{"from": `))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "retry-key-2")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}

	second := doJSONWithKey(t, mux, "POST", "/v1/emails", "retry-key-2", samplePayload())
	if second.Code != http.StatusAccepted {
		t.Fatalf("expected the corrected retry to succeed, got %d: %s", second.Code, second.Body.String())
	}
}

// TestIdempotencyConcurrentIdenticalRequestsCreateOneEmail is the most
// important v0.20 test: many concurrent requests, same tenant/key/
// payload, must resolve to exactly one logical email - arbitrated by
// PostgreSQL, not any in-process coordination.
func TestIdempotencyConcurrentIdenticalRequestsCreateOneEmail(t *testing.T) {
	mux, _, _ := setupMux(t)
	const concurrency = 30
	var wg sync.WaitGroup
	ids := make([]string, concurrency)
	codes := make([]int, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := doJSONWithKey(t, mux, "POST", "/v1/emails", "concurrent-key", samplePayload())
			codes[i] = rec.Code
			if rec.Code == http.StatusAccepted {
				ids[i] = decodeEmail(t, rec).ID
			}
		}(i)
	}
	wg.Wait()

	firstID := ""
	for i, code := range codes {
		if code != http.StatusAccepted {
			t.Fatalf("request %d: expected 202, got %d", i, code)
		}
		if firstID == "" {
			firstID = ids[i]
		} else if ids[i] != firstID {
			t.Fatalf("request %d returned a different email id (%s) than the rest (%s)", i, ids[i], firstID)
		}
	}
}

func TestIdempotencyConcurrentConflictingPayloadsDeterministic(t *testing.T) {
	mux, _, _ := setupMux(t)
	const concurrency = 10
	var wg sync.WaitGroup
	codes := make([]int, concurrency*2)
	for i := 0; i < concurrency; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			codes[i] = doJSONWithKey(t, mux, "POST", "/v1/emails", "conflict-key", samplePayload()).Code
		}(i)
		go func(i int) {
			defer wg.Done()
			payload := samplePayload()
			payload["subject"] = "a different subject entirely"
			codes[concurrency+i] = doJSONWithKey(t, mux, "POST", "/v1/emails", "conflict-key", payload).Code
		}(i)
	}
	wg.Wait()

	accepted, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected status code %d", c)
		}
	}
	if accepted == 0 {
		t.Fatal("expected at least one payload to win ownership of the key")
	}
	if conflicts == 0 {
		t.Fatal("expected the other payload to deterministically conflict, never silently succeed as the winner")
	}
	if accepted+conflicts != len(codes) {
		t.Fatalf("unexpected total: accepted=%d conflicts=%d total=%d", accepted, conflicts, len(codes))
	}
}

func TestIdempotencyAcrossTenantsIndependent(t *testing.T) {
	mux, db, tenantA, authSvcA, keyA := setupMuxNoAuth(t)
	tenantB, err := db.CreateTenant(context.Background(), "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	genB, _, err := authSvcA.Create(context.Background(), tenantB.ID, "b", []string{string(auth.ScopeEmailsSend), string(auth.ScopeEmailsRead)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	muxA := authInjector{next: mux, token: keyA}
	muxB := authInjector{next: mux, token: genB.Raw}

	recA := doJSONWithKey(t, muxA, "POST", "/v1/emails", "shared-key", samplePayload())
	recB := doJSONWithKey(t, muxB, "POST", "/v1/emails", "shared-key", samplePayload())
	if recA.Code != http.StatusAccepted || recB.Code != http.StatusAccepted {
		t.Fatalf("expected both tenants to independently succeed, got %d and %d", recA.Code, recB.Code)
	}
	eA, eB := decodeEmail(t, recA), decodeEmail(t, recB)
	if eA.ID == eB.ID {
		t.Fatal("the same literal idempotency key in two different tenants must not collide")
	}
	_ = tenantA
}

// TestIdempotencySurvivesAPIKeyRotation proves idempotency belongs to the
// TENANT, not the API-key row: a retry authenticated with a NEW key
// (after rotation) must still find the SAME tenant-scoped claim.
func TestIdempotencySurvivesAPIKeyRotation(t *testing.T) {
	mux, _, tenant, authSvc, oldKey := setupMuxNoAuth(t)
	authed := authInjector{next: mux, token: oldKey}

	first := doJSONWithKey(t, authed, "POST", "/v1/emails", "payment-123", samplePayload())
	if first.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", first.Code, first.Body.String())
	}
	e1 := decodeEmail(t, first)

	keys, err := authSvc.List(context.Background(), tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	newGen, _, err := authSvc.Rotate(context.Background(), keys[0].KeyID, 0)
	if err != nil {
		t.Fatal(err)
	}

	authedNew := authInjector{next: mux, token: newGen.Raw}
	second := doJSONWithKey(t, authedNew, "POST", "/v1/emails", "payment-123", samplePayload())
	if second.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", second.Code, second.Body.String())
	}
	e2 := decodeEmail(t, second)
	if e1.ID != e2.ID {
		t.Fatalf("expected the rotated key to find the same tenant-scoped result, got %s then %s", e1.ID, e2.ID)
	}
}

func TestIdempotencyRevokedKeyCannotClaimOrReplay(t *testing.T) {
	mux, _, tenant, authSvc, rawKey := setupMuxNoAuth(t)
	authed := authInjector{next: mux, token: rawKey}

	first := doJSONWithKey(t, authed, "POST", "/v1/emails", "will-be-revoked", samplePayload())
	if first.Code != http.StatusAccepted {
		t.Fatalf("got %d", first.Code)
	}

	keys, err := authSvc.List(context.Background(), tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := authSvc.Revoke(context.Background(), keys[0].KeyID); err != nil {
		t.Fatal(err)
	}

	second := doJSONWithKey(t, authed, "POST", "/v1/emails", "will-be-revoked", samplePayload())
	if second.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked key must not be able to replay an idempotency result, got %d: %s", second.Code, second.Body.String())
	}
}

func TestIdempotencyReplayReflectsCurrentResourceState(t *testing.T) {
	mux, db, _ := setupMux(t)
	first := doJSONWithKey(t, mux, "POST", "/v1/emails", "evolve-key", samplePayload())
	e1 := decodeEmail(t, first)
	if e1.Status != "queued" {
		t.Fatalf("expected initial status queued, got %s", e1.Status)
	}

	delivered := time.Now().UTC()
	if err := db.UpdateMessageStatus(context.Background(), e1.ID, database.StatusDelivered, &delivered); err != nil {
		t.Fatal(err)
	}

	second := doJSONWithKey(t, mux, "POST", "/v1/emails", "evolve-key", samplePayload())
	if second.Code != http.StatusAccepted {
		t.Fatalf("got %d", second.Code)
	}
	e2 := decodeEmail(t, second)
	if e2.Status != "delivered" {
		t.Fatalf("expected the replay to reflect the CURRENT resource state (delivered), got %s", e2.Status)
	}
}

// TestIdempotencyStaleOriginalReplaysReclaimerResultInsteadOfErroring is a
// regression test for a Greptile-flagged bug: when a caller (A) stalls
// past the staleness window and its claim gets reclaimed by a genuinely
// identical retry (B, SAME fingerprint - not a different payload), A
// finally finishing and trying to complete must not surface a 500 -  B's
// result (the only real difference from a normal replay is which of two
// simultaneous identical requests happened to finish the durable work)
// must be replayed to A instead.
func TestIdempotencyStaleOriginalReplaysReclaimerResultInsteadOfErroring(t *testing.T) {
	_, db, tenant, _, _ := setupMuxNoAuth(t)
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := newEmailHandler(db, store)
	ctx := context.Background()
	const op = idempotency.OperationEmailsCreate
	const key = "dup-key"
	const fp = "identical-fingerprint"
	future := time.Now().UTC().Add(time.Hour)

	// A claims first.
	if _, owned, err := db.ClaimIdempotencyKey(ctx, tenant.ID, op, key, fp, future, time.Now().UTC().Add(-time.Minute)); err != nil || !owned {
		t.Fatalf("A's claim: owned=%v err=%v", owned, err)
	}
	// B reclaims it as stale - SAME fingerprint, since B is retrying the
	// identical payload, not a different one.
	if _, owned, err := db.ClaimIdempotencyKey(ctx, tenant.ID, op, key, fp, future, time.Now().UTC().Add(time.Hour)); err != nil || !owned {
		t.Fatalf("B's reclaim: owned=%v err=%v", owned, err)
	}
	// B finishes and completes normally.
	inB := sampleNewMessageFor(t, tenant.ID)
	inB.IdempotencyCompletion = &database.IdempotencyCompletion{Operation: op, IdempotencyKey: key, Fingerprint: fp}
	msgB, err := db.InsertMessage(ctx, inB)
	if err != nil {
		t.Fatalf("B's completion should succeed: %v", err)
	}

	// A, unaware of the reclaim, finally finishes and tries to complete
	// with the SAME fingerprint.
	inA := sampleNewMessageFor(t, tenant.ID)
	inA.IdempotencyCompletion = &database.IdempotencyCompletion{Operation: op, IdempotencyKey: key, Fingerprint: fp}
	if _, err := db.InsertMessage(ctx, inA); !errors.Is(err, database.ErrConflict) {
		t.Fatalf("expected A to lose the completion race with ErrConflict, got %v", err)
	}

	// This is the fix under test: A's handler-level recovery must replay
	// B's result rather than surfacing an error.
	resp, ok := h.replayIfCompleted(ctx, tenant.ID, op, key, fp)
	if !ok {
		t.Fatal("expected replayIfCompleted to find B's completed result")
	}
	if resp.ID != msgB.ID {
		t.Fatalf("expected A to replay B's message id %s, got %s", msgB.ID, resp.ID)
	}
}

func sampleNewMessageFor(t *testing.T, tenantID string) database.NewMessage {
	t.Helper()
	id, err := storage.NewID()
	if err != nil {
		t.Fatal(err)
	}
	to := "to"
	return database.NewMessage{
		ID: id, TenantID: tenantID, MailFrom: "<a@example.com>", FromHeader: "a@example.com",
		Subject: "x", MessageIDHeader: "<x@mailx.local>",
		Recipients: []database.RecipientInput{{Address: "<b@example.com>", HeaderKind: &to}},
	}
}
