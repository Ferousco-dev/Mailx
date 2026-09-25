package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func TestBillingCheckoutRejectsInvalidRequestsBeforePaystack(t *testing.T) {
	b := newBillingAPI(t)
	owner, err := b.svc.SignUp(context.Background(), "Ada", "ada@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}
	org, err := b.svc.CreateOrganization(context.Background(), owner.Human.ID, "Acme", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body, contentType string
		wantStatus              int
		wantCode                string
	}{
		{"missing tenant", `{"plan":"plus"}`, "application/json", http.StatusUnprocessableEntity, "invalid_tenant"},
		{"unknown plan", `{"tenant_id":"` + org.ID + `","plan":"enterprise"}`, "application/json", http.StatusUnprocessableEntity, "invalid_plan"},
		{"malformed JSON", `{"tenant_id":`, "application/json", http.StatusBadRequest, "invalid_json"},
		{"wrong content type", `{"tenant_id":"` + org.ID + `","plan":"plus"}`, "text/plain", http.StatusUnsupportedMediaType, "unsupported_media_type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/billing/checkout", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+owner.AccessToken)
			req.Header.Set("Content-Type", tc.contentType)
			rec := httptest.NewRecorder()
			b.mux.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus || !strings.Contains(rec.Body.String(), `"code":"`+tc.wantCode+`"`) {
				t.Fatalf("status = %d, body = %s; want %d %s", rec.Code, rec.Body, tc.wantStatus, tc.wantCode)
			}
			b.fp.mu.Lock()
			calls := len(b.fp.reqs)
			b.fp.mu.Unlock()
			if calls != 0 {
				t.Fatalf("invalid checkout made %d Paystack calls", calls)
			}
		})
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

func TestBillingWebhookRejectsUnusableSignedCharges(t *testing.T) {
	b := newBillingAPI(t)
	tn := newTestTenant(t, b.db)
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing reference", func(d map[string]any) { d["reference"] = "" }},
		{"missing tenant", func(d map[string]any) { d["metadata"].(map[string]any)["tenant_id"] = "" }},
		{"free plan", func(d map[string]any) { d["metadata"].(map[string]any)["plan"] = "free" }},
		{"unknown plan", func(d map[string]any) { d["metadata"].(map[string]any)["plan"] = "enterprise" }},
		{"failed charge", func(d map[string]any) { d["status"] = "failed" }},
		{"wrong currency", func(d map[string]any) { d["currency"] = "NGN" }},
		{"underpaid", func(d map[string]any) { d["amount"] = 599 }},
		{"unknown tenant", func(d map[string]any) { d["metadata"].(map[string]any)["tenant_id"] = "missing-tenant" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := map[string]any{
				"reference": "ref-" + tc.name, "status": "success", "amount": 600,
				"currency": "USD", "metadata": map[string]any{"tenant_id": tn.ID, "plan": "plus"},
			}
			tc.change(data)
			raw, err := json.Marshal(map[string]any{"event": "charge.success", "data": data})
			if err != nil {
				t.Fatal(err)
			}
			rec := b.webhook(t, raw, b.ps.Sign(raw))
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"ignored"`) {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			tp, err := b.db.GetTenantPlan(context.Background(), tn.ID)
			if err != nil || tp.Plan != billing.PlanFree {
				t.Fatalf("unusable charge changed plan: %+v, %v", tp, err)
			}
		})
	}
}

func TestBillingWebhookRejectsOversizedBodyAndSignedMissingEvent(t *testing.T) {
	b := newBillingAPI(t)
	raw := bytes.Repeat([]byte("x"), maxWebhookBody+1)
	if rec := b.webhook(t, raw, b.ps.Sign(raw)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d %s", rec.Code, rec.Body)
	}
	raw = []byte(`{"data":{}}`)
	if rec := b.webhook(t, raw, b.ps.Sign(raw)); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "malformed_webhook") {
		t.Fatalf("signed body without event: %d %s", rec.Code, rec.Body)
	}
}

func TestPlanLimitAPIError(t *testing.T) {
	if got := planLimitAPIError(errors.New("storage unavailable"), true); got != nil {
		t.Fatalf("non-plan error converted to plan refusal: %+v", got)
	}
	wrapped := fmt.Errorf("database: %w: the free plan allows 500 emails per day", database.ErrPlanLimit)
	volume := planLimitAPIError(wrapped, true)
	if volume == nil || volume.Type != ErrRateLimited || volume.Code != "daily_send_limit_reached" ||
		volume.RetryAfter < 1 || volume.RetryAfter > 24*60*60 ||
		strings.Contains(volume.Message, "database:") || !strings.Contains(volume.Message, "upgrade your plan") {
		t.Fatalf("volume refusal = %+v", volume)
	}
	cap := planLimitAPIError(wrapped, false)
	if cap == nil || cap.Type != ErrForbidden || cap.Code != "plan_limit_reached" || cap.RetryAfter != 0 {
		t.Fatalf("cap refusal = %+v", cap)
	}
}

func TestSecondsUntilUTCMidnight(t *testing.T) {
	for _, tc := range []struct {
		at   time.Time
		want int
	}{
		{time.Date(2026, time.September, 25, 0, 0, 0, 0, time.UTC), 86400},
		{time.Date(2026, time.September, 25, 12, 0, 0, 0, time.UTC), 43200},
		{time.Date(2026, time.September, 25, 23, 59, 59, 500000000, time.UTC), 1},
	} {
		if got := secondsUntilUTCMidnight(tc.at); got != tc.want {
			t.Errorf("secondsUntilUTCMidnight(%s) = %d, want %d", tc.at, got, tc.want)
		}
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

func TestBillingSubscriptionShowsActivePaidPeriod(t *testing.T) {
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
	const period = 30 * 24 * time.Hour
	before := time.Now().UTC()
	if err := b.db.ApplyPlanPayment(ctx, database.Payment{Reference: "ref-active", TenantID: org.ID, Plan: billing.PlanPlus, Amount: 600, Currency: "USD"}, period); err != nil {
		t.Fatal(err)
	}
	rec := b.do(t, http.MethodGet, "/v1/billing/subscription?tenant_id="+org.ID, owner.AccessToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got subscriptionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.TenantID != org.ID || got.Plan != billing.PlanPlus || got.Status != "active" || got.CurrentPeriodEnd == nil {
		t.Fatalf("subscription = %+v", got)
	}
	gotEnd, err := time.Parse(time.RFC3339, *got.CurrentPeriodEnd)
	if err != nil {
		t.Fatal(err)
	}
	if d := gotEnd.Sub(before.Add(period)); d < -5*time.Second || d > 5*time.Second {
		t.Fatalf("current_period_end = %v, want ~= now+%v (before=%v)", gotEnd, period, before)
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
	if err := db.ApplyPlanPayment(ctx, database.Payment{Reference: "r", TenantID: tenantID, Plan: "plus", Amount: 600, Currency: "USD"}, 24*time.Hour); err != nil {
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

func TestBroadcastPlanGate(t *testing.T) {
	a := newDKIMAPI(t)
	f := setupBroadcastReady(t, a, "acme")
	a.db.EnablePlanEnforcement()
	rec := doJSON(t, f.h, "POST", "/v1/broadcasts", f.body(nil))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "plan_limit_reached") {
		t.Fatalf("free broadcast: %d %s", rec.Code, rec.Body)
	}
}
