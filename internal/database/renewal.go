package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Ferousco-dev/mailx/internal/billing"
)

// RenewalSettings is an org's auto-renewal state as shown to its owner.
type RenewalSettings struct {
	AutoRenew  bool
	CardOnFile bool
}

// SetAutoRenew sets tenants.auto_renew. When the value actually changes, the
// current period's reminder marker is cleared so the owner gets a fresh
// reminder that matches the new setting (a "charge is coming" notice is a
// precondition for any charge, DEC-237). ErrNotFound if the tenant is absent.
func (db *DB) SetAutoRenew(ctx context.Context, tenantID string, on bool) (RenewalSettings, error) {
	var s RenewalSettings
	err := db.pool.QueryRow(ctx, `
		UPDATE tenants SET
		       plan_reminder_period_end = CASE WHEN auto_renew <> $2 THEN NULL ELSE plan_reminder_period_end END,
		       plan_reminder_sent_at    = CASE WHEN auto_renew <> $2 THEN NULL ELSE plan_reminder_sent_at END,
		       plan_reminder_auto_renew = CASE WHEN auto_renew <> $2 THEN false ELSE plan_reminder_auto_renew END,
		       auto_renew = $2
		WHERE id = $1
		RETURNING auto_renew, paystack_auth_ciphertext IS NOT NULL`, tenantID, on).Scan(&s.AutoRenew, &s.CardOnFile)
	if err != nil {
		return RenewalSettings{}, normalizeErr(err)
	}
	return s, nil
}

// GetRenewalSettings returns tenantID's auto-renewal state.
func (db *DB) GetRenewalSettings(ctx context.Context, tenantID string) (RenewalSettings, error) {
	var s RenewalSettings
	err := db.pool.QueryRow(ctx,
		`SELECT auto_renew, paystack_auth_ciphertext IS NOT NULL FROM tenants WHERE id = $1`, tenantID,
	).Scan(&s.AutoRenew, &s.CardOnFile)
	if err != nil {
		return RenewalSettings{}, normalizeErr(err)
	}
	return s, nil
}

// SaveRenewalAuthorization stores the ENCRYPTED Paystack authorization (the
// caller seals it with secretbox, associated data = tenant id) and the email
// Paystack's charge_authorization requires alongside it. Plaintext
// authorization codes never reach this package.
func (db *DB) SaveRenewalAuthorization(ctx context.Context, tenantID string, ciphertext, nonce []byte, email string) error {
	if len(ciphertext) == 0 || len(nonce) == 0 || email == "" {
		return errors.New("database: incomplete renewal authorization")
	}
	tag, err := db.pool.Exec(ctx, `
		UPDATE tenants SET paystack_auth_ciphertext = $2, paystack_auth_nonce = $3, paystack_auth_email = $4
		WHERE id = $1`, tenantID, ciphertext, nonce, email)
	if err != nil {
		return fmt.Errorf("database: save renewal authorization: %w", normalizeErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// PlanReminder is one claimed pre-period-end reminder to send.
type PlanReminder struct {
	TenantID   string
	TenantName string
	Plan       string
	PeriodEnd  time.Time
	// AutoRenew is true when the reminder announces a charge (auto_renew on
	// AND a card on file); false means "renew manually or lapse".
	AutoRenew bool
}

// ClaimPlanReminders atomically marks, and returns, every active paid tenant
// whose period ends within (now, now+lead] and has not yet been reminded for
// that exact period end. The single UPDATE ... RETURNING makes the claim
// exactly-once across concurrent tickers. A caller that fails to send must
// call ReleasePlanReminder so the next tick retries.
func (db *DB) ClaimPlanReminders(ctx context.Context, now time.Time, lead time.Duration) ([]PlanReminder, error) {
	rows, err := db.pool.Query(ctx, `
		UPDATE tenants SET plan_reminder_period_end = plan_current_period_end,
		       plan_reminder_sent_at = $1,
		       plan_reminder_auto_renew = (auto_renew AND paystack_auth_ciphertext IS NOT NULL)
		WHERE plan <> 'free' AND plan_status = 'active'
		  AND plan_current_period_end > $1 AND plan_current_period_end <= $1 + make_interval(secs => $2)
		  AND plan_reminder_period_end IS DISTINCT FROM plan_current_period_end
		RETURNING id, name, plan, plan_current_period_end, plan_reminder_auto_renew`, now, lead.Seconds())
	if err != nil {
		return nil, fmt.Errorf("database: claim plan reminders: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []PlanReminder
	for rows.Next() {
		var r PlanReminder
		if err := rows.Scan(&r.TenantID, &r.TenantName, &r.Plan, &r.PeriodEnd, &r.AutoRenew); err != nil {
			return nil, fmt.Errorf("database: scan plan reminder: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReleasePlanReminder undoes a claim whose email could not be sent (only if
// the marker still points at that period).
func (db *DB) ReleasePlanReminder(ctx context.Context, tenantID string, periodEnd time.Time) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE tenants SET plan_reminder_period_end = NULL, plan_reminder_sent_at = NULL, plan_reminder_auto_renew = false
		WHERE id = $1 AND plan_reminder_period_end = $2`, tenantID, periodEnd)
	if err != nil {
		return fmt.Errorf("database: release plan reminder: %w", normalizeErr(err))
	}
	return nil
}

// TenantOwnerEmails returns the email address of every owner of tenantID.
func (db *DB) TenantOwnerEmails(ctx context.Context, tenantID string) ([]string, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT h.email FROM tenant_members m JOIN humans h ON h.id = m.human_id
		WHERE m.tenant_id = $1 AND m.role = 'owner' ORDER BY m.created_at`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("database: tenant owner emails: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RenewalCandidates lists tenants that MAY be due an auto-renewal charge at
// now. It is only a prefilter: ClaimRenewalAttempt re-checks everything
// under the tenant row lock.
func (db *DB) RenewalCandidates(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id FROM tenants
		WHERE auto_renew AND plan <> 'free' AND plan_status = 'active'
		  AND paystack_auth_ciphertext IS NOT NULL
		  AND plan_current_period_end > $1 AND plan_current_period_end <= $1 + make_interval(secs => $2)`,
		now, billing.RenewalChargeWindow.Seconds())
	if err != nil {
		return nil, fmt.Errorf("database: renewal candidates: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RenewalClaim is one claimed (status 'pending', committed) charge attempt.
// Exactly one caller ever receives a given claim; only that caller may send
// the charge to Paystack.
type RenewalClaim struct {
	TenantID       string
	TenantName     string
	Plan           string
	Amount         int64
	PeriodEnd      time.Time
	Attempt        int
	Reference      string
	Email          string
	AuthCiphertext []byte
	AuthNonce      []byte
}

// ClaimRenewalAttempt decides, under SELECT ... FOR UPDATE on the tenant row
// (the lockMemberCap pattern), whether a charge attempt is due and, if so,
// records it as 'pending' before returning. It returns (nil, nil) when no
// charge may be made now. Conditions (all required, DEC-237):
//   - paid plan, status active, auto_renew on, card on file;
//   - now < period end <= now + RenewalChargeWindow;
//   - an auto-renew reminder for THIS period end was sent >= RenewalMinNotice ago;
//   - no pending or succeeded attempt for this period (an unknown outcome
//     blocks all further attempts until reconciled - never re-charge blind);
//   - fewer than RenewalMaxAttempts attempts, the last >= RenewalRetrySpacing ago.
//
// The amount is the SERVER-SIDE price of the tenant's current plan.
func (db *DB) ClaimRenewalAttempt(ctx context.Context, tenantID string, now time.Time) (*RenewalClaim, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("database: begin renewal claim: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		c                                  RenewalClaim
		status                             string
		autoRenew, remindedAutoRenew       bool
		periodEnd, remindedEnd, remindedAt *time.Time
		email                              *string
	)
	err = tx.QueryRow(ctx, `
		SELECT name, plan, plan_status, plan_current_period_end, auto_renew,
		       paystack_auth_ciphertext, paystack_auth_nonce, paystack_auth_email,
		       plan_reminder_period_end, plan_reminder_sent_at, plan_reminder_auto_renew
		FROM tenants WHERE id = $1 FOR UPDATE`, tenantID).Scan(
		&c.TenantName, &c.Plan, &status, &periodEnd, &autoRenew,
		&c.AuthCiphertext, &c.AuthNonce, &email,
		&remindedEnd, &remindedAt, &remindedAutoRenew)
	if err != nil {
		return nil, fmt.Errorf("database: lock tenant for renewal: %w", normalizeErr(err))
	}
	switch {
	case !billing.IsPaid(c.Plan), status != "active", !autoRenew,
		len(c.AuthCiphertext) == 0, len(c.AuthNonce) == 0, email == nil || *email == "",
		periodEnd == nil, !now.Before(*periodEnd), periodEnd.Sub(now) > billing.RenewalChargeWindow,
		remindedEnd == nil, !remindedEnd.Equal(*periodEnd), !remindedAutoRenew,
		remindedAt == nil, now.Sub(*remindedAt) < billing.RenewalMinNotice:
		return nil, nil
	}

	rows, err := tx.Query(ctx, `
		SELECT status, created_at FROM billing_renewal_attempts
		WHERE tenant_id = $1 AND period_end = $2 ORDER BY attempt`, tenantID, *periodEnd)
	if err != nil {
		return nil, fmt.Errorf("database: list renewal attempts: %w", normalizeErr(err))
	}
	n := 0
	var last time.Time
	blocked := false
	for rows.Next() {
		var st string
		if err := rows.Scan(&st, &last); err != nil {
			rows.Close()
			return nil, err
		}
		n++
		if st != "failed" {
			blocked = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if blocked || n >= billing.RenewalMaxAttempts || (n > 0 && now.Sub(last) < billing.RenewalRetrySpacing) {
		return nil, nil
	}

	c.TenantID = tenantID
	c.PeriodEnd = *periodEnd
	c.Attempt = n + 1
	c.Amount = billing.PlanFor(c.Plan).PriceUSDCents
	c.Email = *email
	c.Reference = billing.RenewalReference(tenantID, periodEnd.UnixMicro(), c.Attempt)
	if _, err := tx.Exec(ctx, `
		INSERT INTO billing_renewal_attempts (tenant_id, period_end, attempt, reference, plan, amount, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7)`,
		tenantID, *periodEnd, c.Attempt, c.Reference, c.Plan, c.Amount, now); err != nil {
		return nil, fmt.Errorf("database: record renewal attempt: %w", normalizeErr(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("database: commit renewal claim: %w", normalizeErr(err))
	}
	return &c, nil
}

// FinishRenewalAttempt settles a PENDING attempt as 'succeeded' or 'failed'.
// It never rewrites an already-settled attempt (returns false then).
func (db *DB) FinishRenewalAttempt(ctx context.Context, reference string, succeeded bool, reason string, now time.Time) (bool, error) {
	status := "failed"
	if succeeded {
		status = "succeeded"
	}
	if len(reason) > 500 {
		reason = reason[:500]
	}
	tag, err := db.pool.Exec(ctx, `
		UPDATE billing_renewal_attempts SET status = $2, failure_reason = NULLIF($3, ''), finished_at = $4
		WHERE reference = $1 AND status = 'pending'`, reference, status, reason, now)
	if err != nil {
		return false, fmt.Errorf("database: finish renewal attempt: %w", normalizeErr(err))
	}
	return tag.RowsAffected() == 1, nil
}

// PendingRenewalAttempts returns pending attempts created at or before
// olderThan (outcome unknown; to be verified with Paystack).
func (db *DB) PendingRenewalAttempts(ctx context.Context, olderThan time.Time) ([]RenewalClaim, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT a.tenant_id, t.name, a.plan, a.amount, a.period_end, a.attempt, a.reference
		FROM billing_renewal_attempts a JOIN tenants t ON t.id = a.tenant_id
		WHERE a.status = 'pending' AND a.created_at <= $1 ORDER BY a.created_at`, olderThan)
	if err != nil {
		return nil, fmt.Errorf("database: pending renewal attempts: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []RenewalClaim
	for rows.Next() {
		var c RenewalClaim
		if err := rows.Scan(&c.TenantID, &c.TenantName, &c.Plan, &c.Amount, &c.PeriodEnd, &c.Attempt, &c.Reference); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
