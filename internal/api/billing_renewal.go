package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/billing"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/humanauth"
	"github.com/Ferousco-dev/mailx/internal/observability"
)

// Renewer runs one hourly pass of plan reminders and opt-in auto-renewal
// charges (v0.47 phase 3c, DEC-235..238). Order per pass: reminders, then
// reconcile unknown-outcome charges, then new charges. Lapsing stays in
// database.DowngradeLapsedPlans (called by cmd/mailx after RunOnce).
//
// Every charge is MailX-initiated (Paystack charge_authorization with the
// saved, encrypted authorization) - never Paystack's own subscription engine.
type Renewer struct {
	db     *database.DB
	cfg    *BillingConfig
	mailer humanauth.Mailer
	log    *slog.Logger
}

// NewRenewer builds a Renewer. mailer nil means no reminder can be sent, and
// therefore (by design) no auto-renewal charge is ever made either: a charge
// requires a sent "charge is coming" reminder.
func NewRenewer(db *database.DB, cfg *BillingConfig, mailer humanauth.Mailer, log *slog.Logger) *Renewer {
	if log == nil {
		log = observability.Discard()
	}
	return &Renewer{db: db, cfg: cfg, mailer: mailer, log: log}
}

// RunOnce performs one pass at now. Errors for individual tenants are logged
// and do not stop the pass.
func (r *Renewer) RunOnce(ctx context.Context, now time.Time) {
	if r.mailer == nil {
		return
	}
	r.sendReminders(ctx, now)
	if r.cfg.AuthBox == nil {
		return
	}
	r.reconcilePending(ctx, now)
	ids, err := r.db.RenewalCandidates(ctx, now)
	if err != nil {
		r.log.Warn("billing_renewal_candidates_failed", "error", err.Error())
		return
	}
	for _, id := range ids {
		r.renewOne(ctx, id, now)
	}
}

func usd(cents int64) string { return fmt.Sprintf("$%d.%02d", cents/100, cents%100) }

func (r *Renewer) email(ctx context.Context, tenantID, subject, body string) {
	owners, err := r.db.TenantOwnerEmails(ctx, tenantID)
	if err != nil {
		r.log.Warn("billing_email_owners_failed", "tenant_id", tenantID, "error", err.Error())
		return
	}
	for _, to := range owners {
		if err := r.mailer.SendSystemEmail(ctx, to, subject, body, ""); err != nil {
			r.log.Warn("billing_email_failed", "tenant_id", tenantID, "subject", subject, "error", err.Error())
		}
	}
}

func (r *Renewer) sendReminders(ctx context.Context, now time.Time) {
	claimed, err := r.db.ClaimPlanReminders(ctx, now, billing.ReminderLead)
	if err != nil {
		r.log.Warn("billing_reminder_claim_failed", "error", err.Error())
		return
	}
	for _, rem := range claimed {
		plan := billing.PlanFor(rem.Plan)
		end := rem.PeriodEnd.UTC().Format("2006-01-02 15:04 MST")
		var subject, body string
		if rem.AutoRenew {
			subject = fmt.Sprintf("Your MailX %s plan will renew automatically", plan.ID)
			body = fmt.Sprintf("Your organization %q is on the MailX %s plan, which ends on %s.\n\n"+
				"Automatic renewal is ON: we will charge %s (USD) to your saved card, no earlier than %s, to renew for another 30 days.\n\n"+
				"To avoid this charge, turn off automatic renewal in your billing settings before then.\n",
				rem.TenantName, plan.ID, end, usd(plan.PriceUSDCents),
				rem.PeriodEnd.Add(-billing.RenewalChargeWindow).UTC().Format("2006-01-02 15:04 MST"))
		} else {
			subject = fmt.Sprintf("Your MailX %s plan ends soon", plan.ID)
			body = fmt.Sprintf("Your organization %q is on the MailX %s plan, which ends on %s.\n\n"+
				"Automatic renewal is OFF, so you will not be charged. To keep the plan, renew it (%s for 30 days) from your billing settings before it ends; otherwise the organization moves to the Free plan.\n",
				rem.TenantName, plan.ID, end, usd(plan.PriceUSDCents))
		}
		owners, err := r.db.TenantOwnerEmails(ctx, rem.TenantID)
		sent := 0
		if err == nil {
			for _, to := range owners {
				if serr := r.mailer.SendSystemEmail(ctx, to, subject, body, ""); serr == nil {
					sent++
				} else {
					err = serr
				}
			}
		}
		if sent == 0 {
			// Nobody was told: release so the next pass retries. Without a
			// sent auto-renew reminder no charge can be claimed.
			reason := "no owner"
			if err != nil {
				reason = err.Error()
			}
			r.log.Warn("billing_reminder_send_failed", "tenant_id", rem.TenantID, "error", reason)
			if rerr := r.db.ReleasePlanReminder(ctx, rem.TenantID, rem.PeriodEnd); rerr != nil {
				r.log.Warn("billing_reminder_release_failed", "tenant_id", rem.TenantID, "error", rerr.Error())
			}
			continue
		}
		r.log.Info("billing_reminder_sent", "tenant_id", rem.TenantID, "plan", rem.Plan, "auto_renew", rem.AutoRenew, "period_end", rem.PeriodEnd.UTC().Format(time.RFC3339))
	}
}

func (r *Renewer) renewOne(ctx context.Context, tenantID string, now time.Time) {
	c, err := r.db.ClaimRenewalAttempt(ctx, tenantID, now)
	if err != nil {
		r.log.Warn("billing_renewal_claim_failed", "tenant_id", tenantID, "error", err.Error())
		return
	}
	if c == nil {
		return
	}
	logAttempt := []any{"tenant_id", c.TenantID, "reference", c.Reference, "attempt", c.Attempt, "plan", c.Plan, "amount", c.Amount}
	authCode, err := r.cfg.AuthBox.Decrypt(c.AuthCiphertext, c.AuthNonce, []byte(c.TenantID))
	if err != nil {
		// Nothing was sent to Paystack: this attempt definitively failed.
		r.log.Error("billing_renewal_auth_decrypt_failed", logAttempt...)
		r.settle(ctx, c, billing.ChargeResult{Outcome: billing.ChargeFailed, Reason: "saved card could not be read; please renew manually"}, now)
		return
	}
	r.log.Info("billing_renewal_attempt", logAttempt...)
	res, err := r.cfg.Paystack.ChargeAuthorization(ctx, c.Email, string(authCode), billing.PlanFor(c.Plan), c.Reference,
		billing.Metadata{TenantID: c.TenantID, Plan: c.Plan})
	if err != nil {
		r.log.Warn("billing_renewal_charge_error", append(logAttempt, "error", err.Error())...)
	}
	r.settle(ctx, c, res, now)
}

func (r *Renewer) reconcilePending(ctx context.Context, now time.Time) {
	pending, err := r.db.PendingRenewalAttempts(ctx, now.Add(-billing.RenewalReconcileAfter))
	if err != nil {
		r.log.Warn("billing_renewal_pending_failed", "error", err.Error())
		return
	}
	for i := range pending {
		c := &pending[i]
		res, err := r.cfg.Paystack.VerifyTransaction(ctx, c.Reference)
		if err != nil {
			r.log.Warn("billing_renewal_verify_error", "tenant_id", c.TenantID, "reference", c.Reference, "error", err.Error())
		}
		r.log.Info("billing_renewal_reconciled", "tenant_id", c.TenantID, "reference", c.Reference, "outcome", int(res.Outcome))
		r.settle(ctx, c, res, now)
	}
}

// settle applies a charge result to a pending attempt. Unknown outcomes stay
// pending (blocking any further attempt for the period) until verified.
func (r *Renewer) settle(ctx context.Context, c *database.RenewalClaim, res billing.ChargeResult, now time.Time) {
	logAttempt := []any{"tenant_id", c.TenantID, "reference", c.Reference, "attempt", c.Attempt, "plan", c.Plan, "amount", c.Amount}
	plan := billing.PlanFor(c.Plan)
	switch res.Outcome {
	case billing.ChargeUnknown:
		r.log.Warn("billing_renewal_outcome_unknown", logAttempt...)
		return
	case billing.ChargeSucceeded:
		if res.Amount != c.Amount || !strings.EqualFold(res.Currency, billing.Currency) {
			// Money moved but not what we asked for: never apply, never retry.
			// Stays pending (blocks further charges) and is logged every pass
			// for manual resolution by the operator.
			r.log.Error("billing_renewal_amount_mismatch", append(logAttempt, "paid_amount", res.Amount, "paid_currency", res.Currency)...)
			return
		}
		err := r.db.ApplyPlanPayment(ctx, database.Payment{
			Reference: c.Reference, TenantID: c.TenantID, Plan: c.Plan, Amount: res.Amount, Currency: billing.Currency,
		}, planPeriod)
		if err != nil && !errors.Is(err, database.ErrPaymentAlreadyApplied) {
			// Leave pending: reconcile re-verifies and re-applies (idempotent).
			r.log.Error("billing_renewal_apply_failed", append(logAttempt, "error", err.Error())...)
			return
		}
		if ok, ferr := r.db.FinishRenewalAttempt(ctx, c.Reference, true, "", now); ferr != nil || !ok {
			r.log.Warn("billing_renewal_finish_failed", logAttempt...)
		}
		r.log.Info("billing_renewal_succeeded", append(logAttempt, "already_applied", err != nil)...)
		tp, _ := r.db.GetTenantPlan(ctx, c.TenantID)
		until := "the next 30 days"
		if tp.CurrentPeriodEnd != nil {
			until = tp.CurrentPeriodEnd.UTC().Format("2006-01-02 15:04 MST")
		}
		r.email(ctx, c.TenantID, fmt.Sprintf("Your MailX %s plan was renewed", plan.ID),
			fmt.Sprintf("We charged %s (USD) to your saved card to renew the MailX %s plan for %q. Your plan is now active until %s.\n\nPayment reference: %s\n",
				usd(c.Amount), plan.ID, c.TenantName, until, c.Reference))
	case billing.ChargeFailed:
		reason := strings.TrimSpace(res.Reason)
		if reason == "" {
			reason = "the payment was declined"
		}
		if _, err := r.db.FinishRenewalAttempt(ctx, c.Reference, false, reason, now); err != nil {
			r.log.Error("billing_renewal_finish_failed", append(logAttempt, "error", err.Error())...)
			return
		}
		r.log.Warn("billing_renewal_failed", append(logAttempt, "reason", reason)...)
		next := "If time remains before your plan ends, we will try again in about 12 hours."
		if c.Attempt >= billing.RenewalMaxAttempts {
			next = fmt.Sprintf("This was the final automatic attempt. Your plan ends on %s and the organization will move to the Free plan unless you renew manually from your billing settings.",
				c.PeriodEnd.UTC().Format("2006-01-02 15:04 MST"))
		}
		r.email(ctx, c.TenantID, fmt.Sprintf("Automatic renewal of your MailX %s plan failed", plan.ID),
			fmt.Sprintf("We could not charge your saved card %s (USD) to renew the MailX %s plan for %q (attempt %d of %d).\n\nReason from the payment provider: %s\n\n%s\n",
				usd(c.Amount), plan.ID, c.TenantName, c.Attempt, billing.RenewalMaxAttempts, reason, next))
	}
}
