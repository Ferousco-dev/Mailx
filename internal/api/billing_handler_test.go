package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/billing"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/humanauth"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

const testPaystackSecret = "sk_test_billing_secret"

type fakePaystack struct {
	mu   sync.Mutex
	reqs []map[string]any
}

func (f *fakePaystack) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/transaction/initialize" || r.Header.Get("Authorization") != "Bearer "+testPaystackSecret {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status":false,"message":"bad"}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.reqs = append(f.reqs, body)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"status":true,"message":"ok","data":{"authorization_url":"https://checkout.paystack.com/abc","reference":"ref_abc"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type billingAPI struct {
	mux http.Handler
	db  *database.DB
	svc *humanauth.Service
	ps  *billing.Paystack
	fp  *fakePaystack
}

func newBillingAPI(t *testing.T) billingAPI {
	t.Helper()
	db := newTestDB(t)
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := humanauth.NewService(db, []byte("test-secret-at-least-32-bytes-long!!"))
	if err != nil {
		t.Fatal(err)
	}
	fp := &fakePaystack{}
	ps, err := billing.NewPaystack(testPaystackSecret, fp.server(t).URL)
	if err != nil {
		t.Fatal(err)
	}
	db.EnablePlanEnforcement()
	mux := newMux(newEmailHandler(db, store), auth.NewService(db, nil), func() error { return nil },
		routeServices{humanAuth: svc, billing: &BillingConfig{Paystack: ps}})
	return billingAPI{mux: mux, db: db, svc: svc, ps: ps, fp: fp}
}

func (b billingAPI) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	b.mux.ServeHTTP(rec, req)
	return rec
}

func (b billingAPI) webhook(t *testing.T, raw []byte, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/billing/webhook", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set("x-paystack-signature", sig)
	}
	rec := httptest.NewRecorder()
	b.mux.ServeHTTP(rec, req)
	return rec
}

func chargeSuccess(tenantID, plan, ref string, amount int64) []byte {
	b, _ := json.Marshal(map[string]any{
		"event": "charge.success",
		"data": map[string]any{
			"reference": ref, "status": "success", "amount": amount, "currency": "USD",
			"metadata": map[string]any{"tenant_id": tenantID, "plan": plan},
			"customer": map[string]any{"customer_code": "CUS_x"},
		},
	})
	return b
}

func TestBillingCheckoutOwnerOnly(t *testing.T) {
	b := newBillingAPI(t)
	ctx := context.Background()
	owner, err := b.svc.SignUp(ctx, "Ada", "ada@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}
	org, err := b.svc.CreateOrganization(ctx, owner.Human.ID, "Acme", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := b.svc.SignUp(ctx, "Eve", "eve@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}

	if rec := b.do(t, "POST", "/v1/billing/checkout", "", map[string]string{"tenant_id": org.ID, "plan": "plus"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := b.do(t, "POST", "/v1/billing/checkout", other.AccessToken, map[string]string{"tenant_id": org.ID, "plan": "plus"}); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner: %d %s", rec.Code, rec.Body)
	}
	if rec := b.do(t, "POST", "/v1/billing/checkout", owner.AccessToken, map[string]string{"tenant_id": org.ID, "plan": "free"}); rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
		t.Fatalf("free plan checkout: %d", rec.Code)
	}
	rec := b.do(t, "POST", "/v1/billing/checkout", owner.AccessToken, map[string]string{"tenant_id": org.ID, "plan": "pro"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "https://checkout.paystack.com/abc") {
		t.Fatalf("owner checkout: %d %s", rec.Code, rec.Body)
	}
	if len(b.fp.reqs) != 1 {
		t.Fatalf("paystack calls = %d, want 1 (non-owner must never reach Paystack)", len(b.fp.reqs))
	}
	got := b.fp.reqs[0]
	meta, _ := got["metadata"].(map[string]any)
	if got["email"] != "ada@example.com" || got["currency"] != "USD" || got["amount"] != float64(2400) || meta["tenant_id"] != org.ID || meta["plan"] != "pro" {
		t.Fatalf("unexpected initialize body %+v", got)
	}
}

func TestBillingWebhookSignatureAndReplay(t *testing.T) {
	b := newBillingAPI(t)
	ctx := context.Background()
	tn := newTestTenant(t, b.db)
	body := chargeSuccess(tn.ID, "plus", "ref-1", 600)

	cases := []struct {
		name string
		sig  string
	}{
		{"missing", ""},
		{"not hex", "zzzz"},
		{"wrong key", mustSign(t, "sk_other", body)},
		{"signature of different body", b.ps.Sign(chargeSuccess(tn.ID, "pro", "ref-1", 2400))},
	}
	for _, c := range cases {
		if rec := b.webhook(t, body, c.sig); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: got %d, want 401", c.name, rec.Code)
		}
	}
	if tp, _ := b.db.GetTenantPlan(ctx, tn.ID); tp.Plan != "free" {
		t.Fatalf("unverified webhook changed the plan: %+v", tp)
	}

	if rec := b.webhook(t, body, b.ps.Sign(body)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "applied") {
		t.Fatalf("valid: %d %s", rec.Code, rec.Body)
	}
	tp, _ := b.db.GetTenantPlan(ctx, tn.ID)
	if tp.Plan != "plus" || tp.Status != "active" || tp.CurrentPeriodEnd == nil {
		t.Fatalf("plan not applied: %+v", tp)
	}
	firstEnd := *tp.CurrentPeriodEnd

	if rec := b.webhook(t, body, b.ps.Sign(body)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "already_applied") {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body)
	}
	if tp, _ := b.db.GetTenantPlan(ctx, tn.ID); !tp.CurrentPeriodEnd.Equal(firstEnd) {
		t.Fatal("replay extended the period")
	}

	malformed := []byte(`{"event":`)
	if rec := b.webhook(t, malformed, b.ps.Sign(malformed)); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed signed body: %d", rec.Code)
	}
	// Signed but underpaid / wrong currency / unknown event: acknowledged, not applied.
	under := chargeSuccess(tn.ID, "pro", "ref-under", 600)
	if rec := b.webhook(t, under, b.ps.Sign(under)); rec.Code != http.StatusOK {
		t.Fatalf("underpaid: %d", rec.Code)
	}
	unknown := []byte(`{"event":"subscription.create","data":{}}`)
	if rec := b.webhook(t, unknown, b.ps.Sign(unknown)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ignored") {
		t.Fatalf("unknown event: %d %s", rec.Code, rec.Body)
	}
	if tp, _ := b.db.GetTenantPlan(ctx, tn.ID); tp.Plan != "plus" {
		t.Fatalf("underpaid pro charge changed plan: %+v", tp)
	}
}

func mustSign(t *testing.T, key string, body []byte) string {
	t.Helper()
	p, err := billing.NewPaystack(key, "")
	if err != nil {
		t.Fatal(err)
	}
	return p.Sign(body)
}

func TestBillingSubscriptionMembersOnly(t *testing.T) {
	b := newBillingAPI(t)
	ctx := context.Background()
	owner, _ := b.svc.SignUp(ctx, "Ada", "ada@example.com", "hunter22hunter")
	org, _ := b.svc.CreateOrganization(ctx, owner.Human.ID, "Acme", "")
	other, _ := b.svc.SignUp(ctx, "Eve", "eve@example.com", "hunter22hunter")
	rec := b.do(t, "GET", "/v1/billing/subscription?tenant_id="+org.ID, owner.AccessToken, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"plan":"free"`) {
		t.Fatalf("member: %d %s", rec.Code, rec.Body)
	}
	if rec := b.do(t, "GET", "/v1/billing/subscription?tenant_id="+org.ID, other.AccessToken, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("non-member: %d", rec.Code)
	}
}

// Self-hosted: with no BillingConfig the routes do not exist and nothing is enforced.
func TestBillingRoutesAbsentWhenNotConfigured(t *testing.T) {
	mux, db, _ := setupMux(t)
	if db.PlanEnforcementEnabled() {
		t.Fatal("enforcement must default to off")
	}
	for _, p := range []string{"/v1/billing/webhook", "/v1/billing/checkout"} {
		req := httptest.NewRequest("POST", p, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("%s reachable without billing configured", p)
		}
	}
}

func TestPlanEnforcementAtAPI(t *testing.T) {
	mux, db, tenantID := setupSendMux(t) // example.com verified: 1 domain
	ctx := context.Background()
	send := func() int {
		return doJSON(t, mux, "POST", "/v1/emails", map[string]any{"from": "a@example.com", "to": []string{"x@dest.example"}, "subject": "s", "text": "t"}).Code
	}
	// Off: webhooks allowed on free, sends allowed.
	if c := send(); c != http.StatusAccepted {
		t.Fatalf("send with enforcement off: %d", c)
	}
	db.EnablePlanEnforcement()

	// Webhooks: free refused, plus allowed.
	wh := map[string]any{"url": "http://127.0.0.1:9876/h", "events": []string{"email.failed"}}
	if rec := doJSON(t, mux, "POST", "/v1/webhooks", wh); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "plan_limit_reached") {
		t.Fatalf("free webhook: %d %s", rec.Code, rec.Body)
	}
	// Domains: free cap 5 (1 already).
	for i := 0; i < 4; i++ {
		if rec := doJSON(t, mux, "POST", "/v1/domains", map[string]any{"name": "d" + string(rune('a'+i)) + ".example.org"}); rec.Code != http.StatusCreated {
			t.Fatalf("domain %d under cap: %d %s", i, rec.Code, rec.Body)
		}
	}
	if rec := doJSON(t, mux, "POST", "/v1/domains", map[string]any{"name": "over.example.org"}); rec.Code != http.StatusForbidden {
		t.Fatalf("6th domain: %d %s", rec.Code, rec.Body)
	}
	// Daily volume: fill today to the free cap, then the next send is 429.
	if err := fillToday(ctx, db, tenantID, 500); err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, mux, "POST", "/v1/emails", map[string]any{"from": "a@example.com", "to": []string{"x@dest.example"}, "subject": "s", "text": "t"})
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "daily_send_limit_reached") || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over daily cap: %d %s", rec.Code, rec.Body)
	}
	if err := db.ApplyPlanPayment(ctx, database.Payment{Reference: "r", TenantID: tenantID, Plan: "plus", Amount: 600, Currency: "USD"}, timeNowPlusDay()); err != nil {
		t.Fatal(err)
	}
	if c := send(); c != http.StatusAccepted {
		t.Fatalf("plus send: %d", c)
	}
	// Past the plan gate (this harness's webhook service then rejects the
	// non-HTTPS URL on its own validation, which is fine here).
	if rec := doJSON(t, mux, "POST", "/v1/webhooks", wh); strings.Contains(rec.Body.String(), "plan_limit_reached") {
		t.Fatalf("plus webhook: %d %s", rec.Code, rec.Body)
	}
}

// fillToday inserts messages until the tenant has n created today.
func fillToday(ctx context.Context, db *database.DB, tenantID string, n int) error {
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("fill%028d", i)
		if _, err := db.InsertMessage(ctx, database.NewMessage{
			ID: id, TenantID: tenantID, MailFrom: "<a@example.com>", FromHeader: "a@example.com",
			Subject: "s", MessageIDHeader: "<" + id + "@mailx.local>",
			Recipients: []database.RecipientInput{{Address: "<bob@example.com>"}},
		}); err != nil {
			return err
		}
	}
	return nil
}

func timeNowPlusDay() time.Time { return time.Now().UTC().Add(24 * time.Hour) }

func TestBroadcastPlanGate(t *testing.T) {
	a := newDKIMAPI(t)
	f := setupBroadcastReady(t, a, "acme")
	a.db.EnablePlanEnforcement()
	rec := doJSON(t, f.h, "POST", "/v1/broadcasts", f.body(nil))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "plan_limit_reached") {
		t.Fatalf("free broadcast: %d %s", rec.Code, rec.Body)
	}
}
