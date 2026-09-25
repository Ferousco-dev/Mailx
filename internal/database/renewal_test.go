package database

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/billing"
)

// Racing claims for one tenant period: exactly one wins, exactly one attempt
// row exists, and applying its payment from several goroutines (ticker vs
// webhook) records exactly one billing_payments row and one extension.
func TestClaimRenewalAttemptConcurrentExactlyOnce(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	if err := db.ApplyPlanPayment(ctx, Payment{Reference: "ref_init", TenantID: tn.ID, Plan: "plus", Amount: 600, Currency: "USD"}, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveRenewalAuthorization(ctx, tn.ID, []byte("ct"), []byte("nonce"), "a@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetAutoRenew(ctx, tn.ID, true); err != nil {
		t.Fatal(err)
	}
	tp, _ := db.GetTenantPlan(ctx, tn.ID)
	E := *tp.CurrentPeriodEnd
	if rems, err := db.ClaimPlanReminders(ctx, E.Add(-71*time.Hour), billing.ReminderLead); err != nil || len(rems) != 1 || !rems[0].AutoRenew {
		t.Fatalf("reminder claim: %+v %v", rems, err)
	}
	now := E.Add(-47 * time.Hour)

	const racers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	var claims []*RenewalClaim
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, err := db.ClaimRenewalAttempt(ctx, tn.ID, now)
			if err != nil {
				t.Error(err)
				return
			}
			if c != nil {
				mu.Lock()
				claims = append(claims, c)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(claims) != 1 {
		t.Fatalf("winning claims = %d, want 1", len(claims))
	}
	c := claims[0]
	if c.Amount != 600 || c.Attempt != 1 || c.Plan != "plus" {
		t.Fatalf("claim %+v", c)
	}

	// Ticker and webhook both apply the same renewal reference concurrently.
	var applied, replays int
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := db.ApplyPlanPayment(ctx, Payment{Reference: c.Reference, TenantID: tn.ID, Plan: "plus", Amount: 600, Currency: "USD"}, 30*24*time.Hour)
			mu.Lock()
			defer mu.Unlock()
			switch err {
			case nil:
				applied++
			case ErrPaymentAlreadyApplied:
				replays++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if applied != 1 || replays != racers-1 {
		t.Fatalf("applied=%d replays=%d", applied, replays)
	}
	var payments, attempts int
	_ = db.pool.QueryRow(ctx, `SELECT count(*) FROM billing_payments WHERE tenant_id = $1`, tn.ID).Scan(&payments)
	_ = db.pool.QueryRow(ctx, `SELECT count(*) FROM billing_renewal_attempts WHERE tenant_id = $1`, tn.ID).Scan(&attempts)
	if payments != 2 || attempts != 1 { // ref_init + one renewal
		t.Fatalf("billing_payments=%d attempts=%d, want 2 and 1", payments, attempts)
	}
	tp, _ = db.GetTenantPlan(ctx, tn.ID)
	if !tp.CurrentPeriodEnd.Equal(E.Add(30 * 24 * time.Hour)) {
		t.Fatalf("period end %v, want one extension from %v", tp.CurrentPeriodEnd, E)
	}
	// Pending blocks, settled-once semantics.
	if ok, _ := db.FinishRenewalAttempt(ctx, c.Reference, true, "", now); !ok {
		t.Fatal("finish pending")
	}
	if ok, _ := db.FinishRenewalAttempt(ctx, c.Reference, false, "x", now); ok {
		t.Fatal("a settled attempt must never be rewritten")
	}
}

func TestClaimRenewalAttemptPreconditions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	if err := db.ApplyPlanPayment(ctx, Payment{Reference: "ref_p", TenantID: tn.ID, Plan: "pro", Amount: 2400, Currency: "USD"}, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	tp, _ := db.GetTenantPlan(ctx, tn.ID)
	E := *tp.CurrentPeriodEnd
	now := E.Add(-47 * time.Hour)
	claim := func() *RenewalClaim {
		c, err := db.ClaimRenewalAttempt(ctx, tn.ID, now)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if claim() != nil {
		t.Fatal("claimed with auto_renew off and no card")
	}
	if err := db.SaveRenewalAuthorization(ctx, tn.ID, []byte("ct"), []byte("n"), "a@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetAutoRenew(ctx, tn.ID, true); err != nil {
		t.Fatal(err)
	}
	if claim() != nil {
		t.Fatal("claimed without any reminder")
	}
	if _, err := db.ClaimPlanReminders(ctx, E.Add(-71*time.Hour), billing.ReminderLead); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetAutoRenew(ctx, tn.ID, false); err != nil {
		t.Fatal(err)
	}
	if claim() != nil {
		t.Fatal("claimed after opt-out")
	}
	if _, err := db.SetAutoRenew(ctx, tn.ID, true); err != nil { // reminder reset by toggle
		t.Fatal(err)
	}
	if claim() != nil {
		t.Fatal("claimed although the opt-in reset the reminder")
	}
	if _, err := db.ClaimPlanReminders(ctx, E.Add(-71*time.Hour), billing.ReminderLead); err != nil {
		t.Fatal(err)
	}
	c := claim()
	if c == nil || c.Amount != 2400 {
		t.Fatalf("expected a pro claim at server price, got %+v", c)
	}
}
