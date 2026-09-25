package database

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/billing"
)

func setPlan(t *testing.T, db *DB, tenantID, plan string) {
	t.Helper()
	if _, err := db.pool.Exec(context.Background(), `UPDATE tenants SET plan = $2 WHERE id = $1`, tenantID, plan); err != nil {
		t.Fatal(err)
	}
}

func TestNewTenantDefaultsToFreePlan(t *testing.T) {
	db := newTestDB(t)
	tn := newTestTenant(t, db)
	tp, err := db.GetTenantPlan(context.Background(), tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tp.Plan != "free" || tp.Status != "active" || tp.CurrentPeriodEnd != nil || tp.PaystackCustomerCode != nil {
		t.Fatalf("unexpected default plan state %+v", tp)
	}
}

// Self-hosted (enforcement off): every check is inert even far over the Free caps.
func TestPlanChecksInertWhenEnforcementDisabled(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	for i := 0; i < 6; i++ {
		if _, err := db.CreateDomain(ctx, tn.ID, fmt.Sprintf("d%d.example.com", i), fmt.Sprintf("tok%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CheckDomainLimit(ctx, tn.ID); err != nil {
		t.Fatalf("domain check fired with enforcement off: %v", err)
	}
	if err := db.CheckFeature(ctx, tn.ID, "broadcasts"); err != nil {
		t.Fatalf("broadcast check fired with enforcement off: %v", err)
	}
	if err := db.CheckFeature(ctx, tn.ID, "webhooks"); err != nil {
		t.Fatalf("webhook check fired with enforcement off: %v", err)
	}
	if err := db.CheckDailySendLimit(ctx, tn.ID, 1_000_000); err != nil {
		t.Fatalf("daily check fired with enforcement off: %v", err)
	}
	if err := db.CheckMemberLimit(ctx, tn.ID); err != nil {
		t.Fatalf("member check fired with enforcement off: %v", err)
	}
	if got := db.DefaultRetentionDaysFor("free"); got != DefaultRetentionDays {
		t.Fatalf("retention default with enforcement off = %d, want %d", got, DefaultRetentionDays)
	}
}

func TestDomainLimit(t *testing.T) {
	db := newTestDB(t)
	db.EnablePlanEnforcement()
	ctx := context.Background()
	tn := newTestTenant(t, db)
	for i := 0; i < 5; i++ {
		if err := db.CheckDomainLimit(ctx, tn.ID); err != nil {
			t.Fatalf("domain %d under cap refused: %v", i, err)
		}
		if _, err := db.CreateDomain(ctx, tn.ID, fmt.Sprintf("d%d.example.com", i), fmt.Sprintf("tok%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CheckDomainLimit(ctx, tn.ID); !errors.Is(err, ErrPlanLimit) {
		t.Fatalf("6th domain on free: want ErrPlanLimit, got %v", err)
	}
	setPlan(t, db, tn.ID, "plus")
	if err := db.CheckDomainLimit(ctx, tn.ID); err != nil {
		t.Fatalf("plus allows 15: %v", err)
	}
	setPlan(t, db, tn.ID, "pro")
	if err := db.CheckDomainLimit(ctx, tn.ID); err != nil {
		t.Fatalf("pro is unlimited: %v", err)
	}
}

func TestFeatureGates(t *testing.T) {
	db := newTestDB(t)
	db.EnablePlanEnforcement()
	ctx := context.Background()
	tn := newTestTenant(t, db)
	for _, f := range []string{"broadcasts", "webhooks"} {
		if err := db.CheckFeature(ctx, tn.ID, f); !errors.Is(err, ErrPlanLimit) {
			t.Fatalf("free %s: want ErrPlanLimit, got %v", f, err)
		}
	}
	setPlan(t, db, tn.ID, "plus")
	for _, f := range []string{"broadcasts", "webhooks"} {
		if err := db.CheckFeature(ctx, tn.ID, f); err != nil {
			t.Fatalf("plus %s refused: %v", f, err)
		}
	}
}

func TestDailySendLimit(t *testing.T) {
	db := newTestDB(t)
	db.EnablePlanEnforcement()
	ctx := context.Background()
	tn := newTestTenant(t, db)
	// Seed 499 of today's messages in one statement (free cap 500).
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `
		INSERT INTO messages (id, tenant_id, status, created_at, updated_at, mail_from, subject)
		SELECT m.id || '-' || g, m.tenant_id, m.status, now(), now(), m.mail_from, m.subject
		FROM messages m, generate_series(1, 498) g WHERE m.id = $1`, msg.ID); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.CheckDailySendLimit(ctx, tn.ID, 1); err != nil {
		t.Fatalf("500th message refused: %v", err)
	}
	if err := db.CheckDailySendLimit(ctx, tn.ID, 2); !errors.Is(err, ErrPlanLimit) {
		t.Fatalf("501st message: want ErrPlanLimit, got %v", err)
	}
	// Yesterday's messages do not count toward today.
	if _, err := db.pool.Exec(ctx, `UPDATE messages SET created_at = now() - interval '2 days' WHERE tenant_id = $1`, tn.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckDailySendLimit(ctx, tn.ID, 500); err != nil {
		t.Fatalf("new day refused: %v", err)
	}
	setPlan(t, db, tn.ID, "plus")
	if err := db.CheckDailySendLimit(ctx, tn.ID, 10_000); err != nil {
		t.Fatalf("plus 10k refused: %v", err)
	}
	if err := db.CheckDailySendLimit(ctx, tn.ID, 10_001); !errors.Is(err, ErrPlanLimit) {
		t.Fatalf("plus over 10k: want ErrPlanLimit, got %v", err)
	}
}

func newOwnedOrg(t *testing.T, db *DB, email string) (Human, Tenant) {
	t.Helper()
	h, err := db.CreateHuman(context.Background(), "Owner", email, "x")
	if err != nil {
		t.Fatal(err)
	}
	tn, err := db.CreateOrganization(context.Background(), h.ID, "Org "+email)
	if err != nil {
		t.Fatal(err)
	}
	return h, tn
}

func TestMemberCapAtAccept(t *testing.T) {
	db := newTestDB(t)
	db.EnablePlanEnforcement()
	ctx := context.Background()
	owner, tn := newOwnedOrg(t, db, "owner@example.com")
	now := time.Now().UTC()
	inv, err := db.CreateOrgInvitation(ctx, tn.ID, owner.ID, "a@example.com", "hash-a", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Free: owner only.
	if err := db.CheckMemberLimit(ctx, tn.ID); !errors.Is(err, ErrPlanLimit) {
		t.Fatalf("free invite-time check: want ErrPlanLimit, got %v", err)
	}
	if _, err := db.AcceptOrgInvitationWithSignup(ctx, inv.ID, tn.ID, "A", "a@example.com", "x", now); !errors.Is(err, ErrPlanLimit) {
		t.Fatalf("free accept: want ErrPlanLimit, got %v", err)
	}
	// Refusal rolled back: the invitation is still usable after upgrading.
	setPlan(t, db, tn.ID, "plus")
	if err := db.CheckMemberLimit(ctx, tn.ID); err != nil {
		t.Fatalf("plus invite-time check: %v", err)
	}
	if _, err := db.AcceptOrgInvitationWithSignup(ctx, inv.ID, tn.ID, "A", "a@example.com", "x", now); err != nil {
		t.Fatalf("plus accept after upgrade: %v", err)
	}
	// Existing-human path at the cap is refused too.
	if _, err := db.pool.Exec(ctx, `UPDATE tenants SET plan = 'free' WHERE id = $1`, tn.ID); err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateHuman(ctx, "B", "b@example.com", "x")
	if err != nil {
		t.Fatal(err)
	}
	invB, err := db.CreateOrgInvitation(ctx, tn.ID, owner.ID, "b@example.com", "hash-b", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AcceptOrgInvitationForExistingHuman(ctx, invB.ID, tn.ID, b.ID, now); !errors.Is(err, ErrPlanLimit) {
		t.Fatalf("existing-human accept over cap: want ErrPlanLimit, got %v", err)
	}
}

// Concurrent accepts must not push an org over its cap: the tenant row lock
// serializes them (DEC-223). Plus = 5 members; owner + 4 slots, 8 racers.
func TestMemberCapConcurrentAccepts(t *testing.T) {
	db := newTestDB(t)
	db.EnablePlanEnforcement()
	ctx := context.Background()
	owner, tn := newOwnedOrg(t, db, "owner@example.com")
	setPlan(t, db, tn.ID, "plus")
	now := time.Now().UTC()
	const racers = 8
	ids := make([]string, racers)
	for i := range ids {
		inv, err := db.CreateOrgInvitation(ctx, tn.ID, owner.ID, fmt.Sprintf("r%d@example.com", i), fmt.Sprintf("hash-%d", i), now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = inv.ID
	}
	var wg sync.WaitGroup
	results := make(chan error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := db.AcceptOrgInvitationWithSignup(ctx, ids[i], tn.ID, "R", fmt.Sprintf("r%d@example.com", i), "x", now)
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	ok, limited := 0, 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrPlanLimit):
			limited++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	var members int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM tenant_members WHERE tenant_id = $1`, tn.ID).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if ok != 4 || limited != 4 || members != 5 {
		t.Fatalf("ok=%d limited=%d members=%d; want 4/4/5", ok, limited, members)
	}
}

func TestRetentionDefaultFollowsPlanWhenEnforced(t *testing.T) {
	db := newTestDB(t)
	db.EnablePlanEnforcement()
	ctx := context.Background()
	free := newTestTenant(t, db)
	pro := newTestTenant(t, db)
	setPlan(t, db, pro.ID, "pro")
	fm, err := db.InsertMessage(ctx, sampleNewMessage(t, free.ID))
	if err != nil {
		t.Fatal(err)
	}
	pm, err := db.InsertMessage(ctx, sampleNewMessage(t, pro.ID))
	if err != nil {
		t.Fatal(err)
	}
	tenDaysAgo := time.Now().UTC().Add(-10 * 24 * time.Hour)
	for _, id := range []string{fm.ID, pm.ID} {
		backdateMessage(t, db, id, tenDaysAgo)
		markMessageTerminal(t, db, id, "delivered")
	}
	purged, err := db.PurgeExpiredMessages(ctx, noopDeleteDisk)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 1 || purged[0] != fm.ID {
		t.Fatalf("free (7d) message should be purged, pro (90d) kept; purged=%v", purged)
	}
	if db.DefaultRetentionDaysFor("plus") != billing.PlanFor("plus").RetentionDays {
		t.Fatal("DefaultRetentionDaysFor ignores plan under enforcement")
	}
}

func TestRetentionDefaultFlatWhenNotEnforced(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	free := newTestTenant(t, db)
	fm, err := db.InsertMessage(ctx, sampleNewMessage(t, free.ID))
	if err != nil {
		t.Fatal(err)
	}
	backdateMessage(t, db, fm.ID, time.Now().UTC().Add(-10*24*time.Hour))
	markMessageTerminal(t, db, fm.ID, "delivered")
	purged, err := db.PurgeExpiredMessages(ctx, noopDeleteDisk)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 0 {
		t.Fatalf("self-hosted free tenant must keep the flat 90-day default; purged=%v", purged)
	}
}

func TestApplyPlanPaymentAndReplay(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	end := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
	p := Payment{Reference: "ref-1", TenantID: tn.ID, Plan: "plus", Amount: 600, Currency: "USD", CustomerCode: "CUS_1"}
	if err := db.ApplyPlanPayment(ctx, p, end); err != nil {
		t.Fatal(err)
	}
	tp, _ := db.GetTenantPlan(ctx, tn.ID)
	if tp.Plan != "plus" || tp.Status != "active" || tp.CurrentPeriodEnd == nil || !tp.CurrentPeriodEnd.Equal(end) || tp.PaystackCustomerCode == nil {
		t.Fatalf("unexpected plan after payment: %+v", tp)
	}
	// Replay with a later end must change nothing.
	if err := db.ApplyPlanPayment(ctx, p, end.Add(60*24*time.Hour)); !errors.Is(err, ErrPaymentAlreadyApplied) {
		t.Fatalf("replay: want ErrPaymentAlreadyApplied, got %v", err)
	}
	tp, _ = db.GetTenantPlan(ctx, tn.ID)
	if !tp.CurrentPeriodEnd.Equal(end) {
		t.Fatalf("replay extended the period to %v", tp.CurrentPeriodEnd)
	}
	p.Reference, p.TenantID = "ref-2", "no-such-tenant"
	if err := db.ApplyPlanPayment(ctx, p, end); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown tenant: want ErrNotFound, got %v", err)
	}
	p.TenantID, p.Plan = tn.ID, "free"
	if err := db.ApplyPlanPayment(ctx, p, end); err == nil {
		t.Fatal("free is not purchasable")
	}
}

func TestDowngradeLapsedPlans(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	lapsed := newTestTenant(t, db)
	current := newTestTenant(t, db)
	now := time.Now().UTC()
	if err := db.ApplyPlanPayment(ctx, Payment{Reference: "a", TenantID: lapsed.ID, Plan: "pro", Amount: 2400, Currency: "USD"}, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyPlanPayment(ctx, Payment{Reference: "b", TenantID: current.ID, Plan: "plus", Amount: 600, Currency: "USD"}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	n, err := db.DowngradeLapsedPlans(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("downgraded %d, err %v; want 1", n, err)
	}
	tp, _ := db.GetTenantPlan(ctx, lapsed.ID)
	if tp.Plan != "free" || tp.Status != "lapsed" {
		t.Fatalf("lapsed tenant: %+v", tp)
	}
	tp, _ = db.GetTenantPlan(ctx, current.ID)
	if tp.Plan != "plus" || tp.Status != "active" {
		t.Fatalf("current tenant must be untouched: %+v", tp)
	}
	if n, _ := db.DowngradeLapsedPlans(ctx, now); n != 0 {
		t.Fatalf("second run downgraded %d; want 0 (idempotent)", n)
	}
}
