package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Ferousco-dev/mailx/internal/billing"
	"github.com/jackc/pgx/v5"
)

// ErrPlanLimit is returned (wrapped, with a human-readable reason) when an
// action would exceed the tenant's billing-plan limits. Only ever returned
// while plan enforcement is enabled.
var ErrPlanLimit = errors.New("plan limit reached")

func planLimitf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrPlanLimit}, args...)...)
}

// EnablePlanEnforcement turns on billing-plan limits for every enforcement
// point (DEC-221). cmd/mailx calls it only when MAILX_PAYSTACK_SECRET_KEY is
// configured; it is never turned off at runtime.
func (db *DB) EnablePlanEnforcement() { db.planEnforcement.Store(true) }

// PlanEnforcementEnabled reports whether plan limits apply.
func (db *DB) PlanEnforcementEnabled() bool { return db.planEnforcement.Load() }

// TenantPlan is a tenant's billing state.
type TenantPlan struct {
	Plan                 string
	Status               string
	CurrentPeriodEnd     *time.Time
	PaystackCustomerCode *string
}

// GetTenantPlan returns tenantID's billing state (ErrNotFound if absent).
func (db *DB) GetTenantPlan(ctx context.Context, tenantID string) (TenantPlan, error) {
	var p TenantPlan
	err := db.pool.QueryRow(ctx,
		`SELECT plan, plan_status, plan_current_period_end, paystack_customer_code FROM tenants WHERE id = $1`, tenantID,
	).Scan(&p.Plan, &p.Status, &p.CurrentPeriodEnd, &p.PaystackCustomerCode)
	if err != nil {
		return TenantPlan{}, normalizeErr(err)
	}
	return p, nil
}

// enforcedPlan returns the tenant's plan and true when enforcement is on,
// or (zero, false) without touching the database when it is off.
func (db *DB) enforcedPlan(ctx context.Context, tenantID string) (billing.Plan, bool, error) {
	if !db.PlanEnforcementEnabled() {
		return billing.Plan{}, false, nil
	}
	tp, err := db.GetTenantPlan(ctx, tenantID)
	if err != nil {
		return billing.Plan{}, false, fmt.Errorf("database: tenant plan: %w", err)
	}
	return billing.PlanFor(tp.Plan), true, nil
}

// CheckDailySendLimit refuses (ErrPlanLimit) when accepting adding more
// messages would take the tenant past its plan's daily volume. "Daily" is
// the current UTC calendar day of messages.created_at. The count is bounded
// by the limit itself (same pattern as CountTenantQueued). It is a check,
// not a reservation: concurrent requests can overshoot by at most their own
// batch sizes (DEC-222).
func (db *DB) CheckDailySendLimit(ctx context.Context, tenantID string, adding int) error {
	plan, on, err := db.enforcedPlan(ctx, tenantID)
	if err != nil || !on || plan.DailySends == billing.Unlimited {
		return err
	}
	var n int
	err = db.pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT 1 FROM messages
			WHERE tenant_id = $1 AND created_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
			LIMIT $2
		) s`, tenantID, plan.DailySends).Scan(&n)
	if err != nil {
		return fmt.Errorf("database: count daily sends: %w", normalizeErr(err))
	}
	if n+adding > plan.DailySends {
		return planLimitf("the %s plan allows %d emails per day", plan.ID, plan.DailySends)
	}
	return nil
}

// CheckDomainLimit refuses when the tenant already has its plan's number of
// (non-deleted) domains.
func (db *DB) CheckDomainLimit(ctx context.Context, tenantID string) error {
	plan, on, err := db.enforcedPlan(ctx, tenantID)
	if err != nil || !on || plan.Domains == billing.Unlimited {
		return err
	}
	var n int
	err = db.pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT 1 FROM domains WHERE tenant_id = $1 AND deleted_at IS NULL LIMIT $2
		) s`, tenantID, plan.Domains).Scan(&n)
	if err != nil {
		return fmt.Errorf("database: count domains: %w", normalizeErr(err))
	}
	if !billing.Within(n, plan.Domains) {
		return planLimitf("the %s plan allows %d sending domains", plan.ID, plan.Domains)
	}
	return nil
}

// CheckMemberLimit is the early (invite-send time) member-cap check. The
// authoritative, race-free check runs inside the accept transactions
// (lockMemberCap); see DEC-223.
func (db *DB) CheckMemberLimit(ctx context.Context, tenantID string) error {
	plan, on, err := db.enforcedPlan(ctx, tenantID)
	if err != nil || !on || plan.Members == billing.Unlimited {
		return err
	}
	var n int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM tenant_members WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil {
		return fmt.Errorf("database: count members: %w", normalizeErr(err))
	}
	if !billing.Within(n, plan.Members) {
		return planLimitf("the %s plan allows %d team members", plan.ID, plan.Members)
	}
	return nil
}

// lockMemberCap runs inside an accept transaction before a tenant_members
// insert. It locks the tenant row (SELECT ... FOR UPDATE), so concurrent
// accepts for the same tenant serialize and cannot both pass the count, then
// refuses if humanID is not already a member and the org is at its cap.
func (db *DB) lockMemberCap(ctx context.Context, tx pgx.Tx, tenantID, humanID string) error {
	if !db.PlanEnforcementEnabled() {
		return nil
	}
	var planID string
	if err := tx.QueryRow(ctx, `SELECT plan FROM tenants WHERE id = $1 FOR UPDATE`, tenantID).Scan(&planID); err != nil {
		return fmt.Errorf("database: lock tenant for member cap: %w", normalizeErr(err))
	}
	plan := billing.PlanFor(planID)
	if plan.Members == billing.Unlimited {
		return nil
	}
	var n int
	var already bool
	if err := tx.QueryRow(ctx, `
		SELECT count(*), coalesce(bool_or(human_id = $2), false) FROM tenant_members WHERE tenant_id = $1`,
		tenantID, humanID).Scan(&n, &already); err != nil {
		return fmt.Errorf("database: count members: %w", normalizeErr(err))
	}
	if already {
		return nil
	}
	if !billing.Within(n, plan.Members) {
		return planLimitf("the %s plan allows %d team members", plan.ID, plan.Members)
	}
	return nil
}

// CheckFeature refuses when the tenant's plan has the feature disabled.
// feature is "broadcasts" or "webhooks".
func (db *DB) CheckFeature(ctx context.Context, tenantID, feature string) error {
	plan, on, err := db.enforcedPlan(ctx, tenantID)
	if err != nil || !on {
		return err
	}
	var allowed bool
	switch feature {
	case "broadcasts":
		allowed = plan.Broadcasts
	case "webhooks":
		allowed = plan.Webhooks
	default:
		return fmt.Errorf("database: unknown plan feature %q", feature)
	}
	if !allowed {
		return planLimitf("%s are not available on the %s plan", feature, plan.ID)
	}
	return nil
}

// IsTenantMember reports whether humanID belongs to tenantID in any role.
func (db *DB) IsTenantMember(ctx context.Context, tenantID, humanID string) (bool, error) {
	var ok bool
	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM tenant_members WHERE tenant_id = $1 AND human_id = $2)`, tenantID, humanID,
	).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("database: is tenant member: %w", normalizeErr(err))
	}
	return ok, nil
}

// ErrPaymentAlreadyApplied means this Paystack reference was already applied
// (a webhook replay or Paystack retry); nothing changed.
var ErrPaymentAlreadyApplied = errors.New("database: payment already applied")

// Payment is one verified Paystack charge to apply.
type Payment struct {
	Reference    string
	TenantID     string
	Plan         string
	Amount       int64
	Currency     string
	CustomerCode string
}

// ApplyPlanPayment atomically records the payment reference (primary key:
// a replay is ErrPaymentAlreadyApplied) and extends the tenant's plan by
// period. ErrNotFound if the tenant does not exist.
//
// A renewal of the SAME plan while still active extends from the LATER of
// now and the current plan_current_period_end, rather than overwriting it
// from now - otherwise an owner renewing a few days early would simply
// lose those remaining paid days (CodeRabbit, PR #24). A plan CHANGE
// (upgrade/downgrade) or a renewal after the plan had already lapsed
// starts a fresh period from now instead: carrying over remaining time
// priced under a DIFFERENT plan has no well-defined meaning here.
func (db *DB) ApplyPlanPayment(ctx context.Context, p Payment, period time.Duration) error {
	if !billing.IsPaid(p.Plan) {
		return fmt.Errorf("database: plan %q is not purchasable", p.Plan)
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin apply payment: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var customer *string
	if p.CustomerCode != "" {
		customer = &p.CustomerCode
	}
	tag, err := tx.Exec(ctx, `
		UPDATE tenants SET plan = $2, plan_status = 'active',
		       plan_current_period_end = GREATEST(now(),
		           CASE WHEN plan = $2 AND plan_status = 'active' AND plan_current_period_end > now()
		                THEN plan_current_period_end ELSE now() END
		       ) + make_interval(secs => $3),
		       paystack_customer_code = COALESCE($4, paystack_customer_code)
		WHERE id = $1`, p.TenantID, p.Plan, period.Seconds(), customer)
	if err != nil {
		return fmt.Errorf("database: apply plan: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	tag, err = tx.Exec(ctx, `
		INSERT INTO billing_payments (reference, tenant_id, plan, amount, currency)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (reference) DO NOTHING`,
		p.Reference, p.TenantID, p.Plan, p.Amount, p.Currency)
	if err != nil {
		return fmt.Errorf("database: record payment: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrPaymentAlreadyApplied // rollback also undoes the UPDATE above
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit apply payment: %w", normalizeErr(err))
	}
	return nil
}

// DowngradeLapsedPlans moves every paid tenant whose period ended before now
// back to free with status 'lapsed', returning how many changed. MVP: no
// automatic renewal charge is attempted (RSK-044).
//
// Before clearing plan, it PINS retention_days explicitly to the lapsing
// plan's own window (COALESCE: only when the tenant has no existing
// explicit override, which always wins per this column's normal
// convention). Without this, a tenant that had retention_days = NULL
// (relying on their paid plan's 30/90-day default) would silently drop to
// Free's 7-day default the instant they lapse, and the next
// retention-purge run would irreversibly hard-delete anything between 7
// days and their old window (data-loss finding, PR #24 review) - a
// renewal running even one hour late would be enough to trigger it.
// Pinning the window here means a lapse only ever changes billing state,
// never retention behavior.
func (db *DB) DowngradeLapsedPlans(ctx context.Context, now time.Time) (int64, error) {
	tag, err := db.pool.Exec(ctx, `
		UPDATE tenants SET
		       retention_days = COALESCE(retention_days,
		           CASE plan WHEN 'plus' THEN 30 WHEN 'pro' THEN 90 END),
		       plan = 'free', plan_status = 'lapsed'
		WHERE plan <> 'free' AND plan_current_period_end < $1`, now)
	if err != nil {
		return 0, fmt.Errorf("database: downgrade lapsed plans: %w", normalizeErr(err))
	}
	return tag.RowsAffected(), nil
}
