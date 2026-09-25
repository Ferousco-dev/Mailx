package api

import (
	"context"
	"encoding/json"
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
	"github.com/Ferousco-dev/mailx/internal/secretbox"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

// fakeChargeServer imitates Paystack's charge_authorization and verify
// endpoints. mode: "success", "decline", "error500" (charge taken but the
// response is a 5xx - the ambiguous case).
type fakeChargeServer struct {
	mu      sync.Mutex
	mode    string
	delay   time.Duration
	charges []map[string]any
	charged map[string]bool // references Paystack actually charged
	refs    map[string]int  // every reference ever submitted (dup detection)
}

func (f *fakeChargeServer) server(t *testing.T) *httptest.Server {
	f.charged, f.refs = map[string]bool{}, map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testPaystackSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/transaction/charge_authorization":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			ref, _ := body["reference"].(string)
			f.refs[ref]++
			if f.refs[ref] > 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"status":false,"message":"Duplicate Transaction Reference"}`))
				return
			}
			f.charges = append(f.charges, body)
			amt := int64(body["amount"].(float64))
			switch f.mode {
			case "decline":
				_, _ = w.Write([]byte(`{"status":true,"message":"Charge attempted","data":{"status":"failed","reference":"` + ref + `","amount":` + itoa(amt) + `,"currency":"USD","gateway_response":"Insufficient Funds"}}`))
			case "error500":
				f.charged[ref] = true
				w.WriteHeader(http.StatusBadGateway)
			default:
				f.charged[ref] = true
				_, _ = w.Write([]byte(`{"status":true,"message":"Charge attempted","data":{"status":"success","reference":"` + ref + `","amount":` + itoa(amt) + `,"currency":"USD","gateway_response":"Approved"}}`))
			}
		case strings.HasPrefix(r.URL.Path, "/transaction/verify/"):
			ref := strings.TrimPrefix(r.URL.Path, "/transaction/verify/")
			if f.mode == "verify401" {
				// A rotated/misconfigured secret key, or any transient
				// auth/proxy failure on the VERIFY call itself - this says
				// nothing about whether the underlying charge went through.
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"status":false,"message":"Invalid key"}`))
				return
			}
			if !f.charged[ref] {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"status":false,"message":"Transaction reference not found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":true,"message":"Verification successful","data":{"status":"success","reference":"` + ref + `","amount":600,"currency":"USD","gateway_response":"Approved"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func (f *fakeChargeServer) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.charges) }

type sentMail struct{ to, subject, body string }

type captureMailer struct {
	mu   sync.Mutex
	sent []sentMail
}

func (m *captureMailer) SendSystemEmail(_ context.Context, to, subject, text, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentMail{to, subject, text})
	return nil
}

func (m *captureMailer) matching(sub string) []sentMail {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sentMail
	for _, s := range m.sent {
		if strings.Contains(s.subject, sub) {
			out = append(out, s)
		}
	}
	return out
}

type renewalFixture struct {
	db      *database.DB
	svc     *humanauth.Service
	cfg     *BillingConfig
	mux     http.Handler
	fc      *fakeChargeServer
	mailer  *captureMailer
	renewer *Renewer
	orgID   string
	owner   humanauth.Session
	end     time.Time // current period end after the initial checkout
}

const testAuthCode = "AUTH_supersecret_code"

// newRenewalFixture: an owner with a Plus org paid via checkout (period E),
// with a reusable card saved through the real webhook path.
func newRenewalFixture(t *testing.T, withBox bool) *renewalFixture {
	t.Helper()
	db := newTestDB(t)
	db.EnablePlanEnforcement()
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := humanauth.NewService(db, []byte("test-secret-at-least-32-bytes-long!!"))
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeChargeServer{}
	ps, err := billing.NewPaystack(testPaystackSecret, fc.server(t).URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &BillingConfig{Paystack: ps}
	if withBox {
		box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.AuthBox = box
	}
	mux := newMux(newEmailHandler(db, store), auth.NewService(db, nil), func() error { return nil },
		routeServices{humanAuth: svc, billing: cfg})
	mailer := &captureMailer{}
	f := &renewalFixture{db: db, svc: svc, cfg: cfg, mux: mux, fc: fc, mailer: mailer, renewer: NewRenewer(db, cfg, mailer, nil)}
	ctx := context.Background()
	f.owner, err = svc.SignUp(ctx, "Ada", "ada@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}
	org, err := svc.CreateOrganization(ctx, f.owner.Human.ID, "Acme", "")
	if err != nil {
		t.Fatal(err)
	}
	f.orgID = org.ID
	raw := []byte(`{"event":"charge.success","data":{"reference":"ref_checkout_1","status":"success","amount":600,"currency":"USD",
		"metadata":{"tenant_id":"` + org.ID + `","plan":"plus"},"customer":{"customer_code":"CUS_1","email":"ada@example.com"},
		"authorization":{"authorization_code":"` + testAuthCode + `","reusable":true}}}`)
	if rec := f.webhook(t, raw); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "applied") {
		t.Fatalf("checkout webhook: %d %s", rec.Code, rec.Body)
	}
	f.end = f.periodEnd(t)
	return f
}

func (f *renewalFixture) webhook(t *testing.T, raw []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/billing/webhook", strings.NewReader(string(raw)))
	req.Header.Set("x-paystack-signature", f.cfg.Paystack.Sign(raw))
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func (f *renewalFixture) patch(t *testing.T, token string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PATCH", "/v1/billing/auto-renew", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func (f *renewalFixture) periodEnd(t *testing.T) time.Time {
	t.Helper()
	tp, err := f.db.GetTenantPlan(context.Background(), f.orgID)
	if err != nil || tp.CurrentPeriodEnd == nil {
		t.Fatalf("plan: %+v %v", tp, err)
	}
	return *tp.CurrentPeriodEnd
}

func (f *renewalFixture) enable(t *testing.T) {
	t.Helper()
	if _, err := f.db.SetAutoRenew(context.Background(), f.orgID, true); err != nil {
		t.Fatal(err)
	}
}

func TestAutoRenewToggleOwnerOnlyAndNeverCharges(t *testing.T) {
	f := newRenewalFixture(t, true)
	other, err := f.svc.SignUp(context.Background(), "Eve", "eve@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}
	on := `{"tenant_id":"` + f.orgID + `","auto_renew":true,"amount":1,"plan":"pro"}`
	if rec := f.patch(t, "", on); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := f.patch(t, other.AccessToken, on); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner: %d %s", rec.Code, rec.Body)
	}
	if rec := f.patch(t, f.owner.AccessToken, `{"tenant_id":"`+f.orgID+`"}`); rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
		t.Fatalf("missing auto_renew: %d", rec.Code)
	}
	rs, _ := f.db.GetRenewalSettings(context.Background(), f.orgID)
	if rs.AutoRenew || !rs.CardOnFile {
		t.Fatalf("default must be off with card saved from checkout: %+v", rs)
	}
	rec := f.patch(t, f.owner.AccessToken, on)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"auto_renew":true`) || !strings.Contains(rec.Body.String(), `"card_on_file":true`) {
		t.Fatalf("owner enable: %d %s", rec.Code, rec.Body)
	}
	// The toggle never charges, and client-supplied amount/plan are ignored.
	if f.fc.count() != 0 {
		t.Fatalf("toggle triggered %d charges", f.fc.count())
	}
	if tp, _ := f.db.GetTenantPlan(context.Background(), f.orgID); tp.Plan != "plus" {
		t.Fatalf("toggle changed plan: %+v", tp)
	}

	// Without MAILX_BILLING_MASTER_KEY auto-renew cannot be enabled, and no
	// card is ever stored.
	g := newRenewalFixture(t, false)
	if rec := g.patch(t, g.owner.AccessToken, `{"tenant_id":"`+g.orgID+`","auto_renew":true}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no box: %d %s", rec.Code, rec.Body)
	}
	if rs, _ := g.db.GetRenewalSettings(context.Background(), g.orgID); rs.CardOnFile || rs.AutoRenew {
		t.Fatalf("no box must store nothing: %+v", rs)
	}
}

func TestRenewalReminderExactlyOncePerPeriod(t *testing.T) {
	for _, autoRenew := range []bool{false, true} {
		f := newRenewalFixture(t, true)
		if autoRenew {
			f.enable(t)
		}
		ctx := context.Background()
		f.renewer.RunOnce(ctx, f.end.Add(-80*time.Hour))
		if n := len(f.mailer.sent); n != 0 {
			t.Fatalf("reminder too early: %d", n)
		}
		for _, h := range []int{71, 70, 60, 50} {
			f.renewer.RunOnce(ctx, f.end.Add(-time.Duration(h)*time.Hour))
		}
		want := "ends soon"
		if autoRenew {
			want = "will renew automatically"
		}
		got := f.mailer.matching("plan")
		reminders := f.mailer.matching(want)
		if len(reminders) != 1 || reminders[0].to != "ada@example.com" {
			t.Fatalf("auto_renew=%v: want exactly 1 %q reminder, got %+v", autoRenew, want, got)
		}
		if autoRenew && !strings.Contains(reminders[0].body, "$6.00") {
			t.Fatalf("auto-renew reminder must state the amount: %s", reminders[0].body)
		}
		if !autoRenew && f.fc.count() != 0 {
			t.Fatalf("auto-renew off must never charge: %d", f.fc.count())
		}
	}
}

func TestAutoRenewSuccessExtendsFromPeriodEnd(t *testing.T) {
	f := newRenewalFixture(t, true)
	f.enable(t)
	ctx := context.Background()
	E := f.end
	f.renewer.RunOnce(ctx, E.Add(-71*time.Hour)) // reminder
	f.renewer.RunOnce(ctx, E.Add(-49*time.Hour)) // outside charge window
	if f.fc.count() != 0 {
		t.Fatal("charged before the charge window")
	}
	f.renewer.RunOnce(ctx, E.Add(-47*time.Hour))
	if f.fc.count() != 1 {
		t.Fatalf("charges = %d, want 1", f.fc.count())
	}
	c := f.fc.charges[0]
	if c["amount"] != float64(600) || c["currency"] != "USD" || c["authorization_code"] != testAuthCode || c["email"] != "ada@example.com" ||
		!strings.HasPrefix(c["reference"].(string), "mailx-renew-"+f.orgID+"-") {
		t.Fatalf("unexpected charge body %+v", c)
	}
	// Same-plan renewal extends from E (DEC-227), not from "now".
	if got := f.periodEnd(t); !got.Equal(E.Add(planPeriod)) {
		t.Fatalf("period end %v, want %v", got, E.Add(planPeriod))
	}
	if len(f.mailer.matching("was renewed")) != 1 {
		t.Fatalf("missing success email: %+v", f.mailer.sent)
	}
	// Paystack's own charge.success webhook for the same reference is a replay.
	ref := c["reference"].(string)
	raw := []byte(`{"event":"charge.success","data":{"reference":"` + ref + `","status":"success","amount":600,"currency":"USD","metadata":{"tenant_id":"` + f.orgID + `","plan":"plus"}}}`)
	if rec := f.webhook(t, raw); !strings.Contains(rec.Body.String(), "already_applied") {
		t.Fatalf("renewal webhook must be a replay: %s", rec.Body)
	}
	for _, h := range []int{46, 30, 20, 1} {
		f.renewer.RunOnce(ctx, E.Add(-time.Duration(h)*time.Hour))
	}
	if f.fc.count() != 1 || !f.periodEnd(t).Equal(E.Add(planPeriod)) {
		t.Fatalf("extra charges/extensions: charges=%d end=%v", f.fc.count(), f.periodEnd(t))
	}
}

func TestAutoRenewDeclinesAreBoundedThenLapseWithRetentionPinned(t *testing.T) {
	f := newRenewalFixture(t, true)
	f.fc.mode = "decline"
	f.enable(t)
	ctx := context.Background()
	E := f.end
	for _, h := range []int{71, 47, 46, 40, 35, 30, 23, 12, 2} {
		f.renewer.RunOnce(ctx, E.Add(-time.Duration(h)*time.Hour))
	}
	if f.fc.count() != billing.RenewalMaxAttempts {
		t.Fatalf("charge attempts = %d, want %d", f.fc.count(), billing.RenewalMaxAttempts)
	}
	fails := f.mailer.matching("failed")
	if len(fails) != billing.RenewalMaxAttempts || !strings.Contains(fails[0].body, "Insufficient Funds") ||
		!strings.Contains(fails[len(fails)-1].body, "final automatic attempt") {
		t.Fatalf("failure emails: %+v", fails)
	}
	for _, m := range f.mailer.sent {
		if strings.Contains(m.body, testAuthCode) {
			t.Fatal("authorization code leaked into an email")
		}
	}
	if !f.periodEnd(t).Equal(E) {
		t.Fatal("declined renewal must not extend the period")
	}
	if n, err := f.db.DowngradeLapsedPlans(ctx, E.Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("lapse: %d %v", n, err)
	}
	tn, err := f.db.GetTenant(ctx, f.orgID)
	if err != nil {
		t.Fatal(err)
	}
	if tp, _ := f.db.GetTenantPlan(ctx, f.orgID); tp.Plan != "free" || tp.Status != "lapsed" {
		t.Fatalf("not lapsed: %+v", tp)
	}
	if tn.RetentionDays == nil || *tn.RetentionDays != 30 {
		t.Fatalf("retention must be pinned to plus's 30 days on lapse, got %v", tn.RetentionDays)
	}
	f.renewer.RunOnce(ctx, E.Add(2*time.Hour))
	if f.fc.count() != billing.RenewalMaxAttempts {
		t.Fatal("lapsed tenant was charged")
	}
}

func TestAutoRenewRequiresNoticeAfterLateOptIn(t *testing.T) {
	f := newRenewalFixture(t, true)
	ctx := context.Background()
	E := f.end
	f.renewer.RunOnce(ctx, E.Add(-71*time.Hour)) // "ends soon" (auto-renew off)
	f.enable(t)                                  // late opt-in resets the reminder
	f.renewer.RunOnce(ctx, E.Add(-30*time.Hour)) // "will renew" reminder; too soon to charge
	f.renewer.RunOnce(ctx, E.Add(-10*time.Hour))
	if f.fc.count() != 0 {
		t.Fatalf("charged with < %v notice", billing.RenewalMinNotice)
	}
	if len(f.mailer.matching("will renew automatically")) != 1 {
		t.Fatalf("late opt-in reminder missing: %+v", f.mailer.sent)
	}
	f.renewer.RunOnce(ctx, E.Add(-5*time.Hour))
	if f.fc.count() != 1 {
		t.Fatalf("charges = %d, want 1 after notice elapsed", f.fc.count())
	}
}

func TestAutoRenewUnknownOutcomeIsReconciledNeverRecharged(t *testing.T) {
	f := newRenewalFixture(t, true)
	f.fc.mode = "error500" // Paystack took the money, but we got a 502
	f.enable(t)
	ctx := context.Background()
	E := f.end
	f.renewer.RunOnce(ctx, E.Add(-71*time.Hour))
	f.renewer.RunOnce(ctx, E.Add(-47*time.Hour))
	f.renewer.RunOnce(ctx, E.Add(-47*time.Hour+30*time.Minute)) // too fresh to verify, and pending blocks
	if f.fc.count() != 1 || !f.periodEnd(t).Equal(E) {
		t.Fatalf("after ambiguous charge: charges=%d end=%v", f.fc.count(), f.periodEnd(t))
	}
	f.renewer.RunOnce(ctx, E.Add(-35*time.Hour)) // verify -> success -> apply once
	if f.fc.count() != 1 {
		t.Fatalf("ambiguous charge was retried: %d", f.fc.count())
	}
	if got := f.periodEnd(t); !got.Equal(E.Add(planPeriod)) {
		t.Fatalf("reconciled renewal: end %v want %v", got, E.Add(planPeriod))
	}
	f.renewer.RunOnce(ctx, E.Add(-20*time.Hour))
	if f.fc.count() != 1 || !f.periodEnd(t).Equal(E.Add(planPeriod)) {
		t.Fatal("reconciled payment applied twice or re-charged")
	}
}

// TestAutoRenewVerify401NeverDoubleCharges is a regression test for
// CodeRabbit's finding (PR #26): a charge whose outcome was ambiguous (here,
// a 502 from charge_authorization) must stay pending - never re-charged under
// a fresh reference - when the LATER verify call itself fails with a
// transient error (401 from a rotated key, here) rather than an authoritative
// "Paystack has no record of this reference" response. Misclassifying that
// verify failure as "definitely not charged" would let a fresh attempt fire
// and double-charge the card if the original charge actually succeeded.
func TestAutoRenewVerify401NeverDoubleCharges(t *testing.T) {
	f := newRenewalFixture(t, true)
	f.fc.mode = "error500" // ambiguous: charge_authorization "took the money" but answered 502
	f.enable(t)
	ctx := context.Background()
	E := f.end
	f.renewer.RunOnce(ctx, E.Add(-71*time.Hour))
	f.renewer.RunOnce(ctx, E.Add(-47*time.Hour)) // fires the ambiguous charge, attempt stays pending
	if f.fc.count() != 1 {
		t.Fatalf("expected exactly one charge attempt, got %d", f.fc.count())
	}
	f.fc.mode = "verify401" // verify itself now fails with an auth error
	f.renewer.RunOnce(ctx, E.Add(-35*time.Hour))
	if f.fc.count() != 1 {
		t.Fatalf("a failed verify call caused a second charge attempt: %d", f.fc.count())
	}
	// The period must NOT have been extended (the ambiguous charge was never
	// applied) and must NOT be lapsed either - it stays pending for a human
	// to resolve, exactly as the amount-mismatch case does.
	if got := f.periodEnd(t); !got.Equal(E) {
		t.Fatalf("period end changed to %v while the charge outcome was still unresolved", got)
	}
}

// N concurrent renewal passes (e.g. several replicas' tickers firing at once)
// for the same tenant and instant: exactly one charge, one extension.
func TestAutoRenewConcurrentPassesChargeOnce(t *testing.T) {
	f := newRenewalFixture(t, true)
	f.enable(t)
	f.fc.delay = 30 * time.Millisecond
	ctx := context.Background()
	E := f.end
	f.renewer.RunOnce(ctx, E.Add(-71*time.Hour))
	const racers = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			NewRenewer(f.db, f.cfg, f.mailer, nil).RunOnce(ctx, E.Add(-47*time.Hour))
		}()
	}
	close(start)
	wg.Wait()
	if f.fc.count() != 1 {
		t.Fatalf("charges = %d, want exactly 1", f.fc.count())
	}
	for ref, n := range f.fc.refs {
		if n != 1 {
			t.Fatalf("reference %s submitted %d times", ref, n)
		}
	}
	if got := f.periodEnd(t); !got.Equal(E.Add(planPeriod)) {
		t.Fatalf("period end %v, want exactly one extension to %v", got, E.Add(planPeriod))
	}
	if len(f.mailer.matching("was renewed")) != 1 {
		t.Fatalf("success emails: %d", len(f.mailer.matching("was renewed")))
	}
}
