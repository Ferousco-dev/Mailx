package main

import (
	"context"
	"os"
	"time"

	"github.com/Ferousco-dev/mailx/internal/api"
	"github.com/Ferousco-dev/mailx/internal/billing"
	"github.com/Ferousco-dev/mailx/internal/database"
)

// buildBillingConfig wires Paystack billing from MAILX_PAYSTACK_SECRET_KEY.
// Unset (the self-hosted default) returns nil: billing routes are not
// registered and plan enforcement stays OFF, so a self-hosted tenant keeps
// its pre-billing unlimited behavior (DEC-221). When set, it also turns on
// plan enforcement on db — the single switch every enforcement point reads.
//
// MAILX_PAYSTACK_PUBLIC_KEY is for the dashboard frontend only; MailX never
// needs it server-side. MAILX_PAYSTACK_CALLBACK_URL (optional) is where
// Paystack returns the customer after checkout. MAILX_PAYSTACK_BASE_URL
// (optional) overrides the API root for testing.
func buildBillingConfig(db *database.DB) (*api.BillingConfig, error) {
	secret := os.Getenv("MAILX_PAYSTACK_SECRET_KEY")
	if secret == "" {
		return nil, nil
	}
	ps, err := billing.NewPaystack(secret, os.Getenv("MAILX_PAYSTACK_BASE_URL"))
	if err != nil {
		return nil, err
	}
	db.EnablePlanEnforcement()
	return &api.BillingConfig{Paystack: ps, CallbackURL: os.Getenv("MAILX_PAYSTACK_CALLBACK_URL")}, nil
}

// enablePlanEnforcementFromEnv is the admin-CLI equivalent (no routes): it
// keeps `mailx purge-retention`/`show-retention` consistent with the server.
func enablePlanEnforcementFromEnv(db *database.DB) {
	if os.Getenv("MAILX_PAYSTACK_SECRET_KEY") != "" {
		db.EnablePlanEnforcement()
	}
}

// planLapseInterval is how often lapsed paid plans are downgraded.
const planLapseInterval = time.Hour

// runPlanLapse downgrades paid tenants whose period has ended. MVP LIMITATION
// (RSK-044): Plus/Pro do NOT auto-renew. No recurring charge is attempted;
// the owner must run checkout again each 30-day cycle or drop to Free.
func runPlanLapse(ctx context.Context, db *database.DB, o obs) error {
	ticker := time.NewTicker(planLapseInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			n, err := db.DowngradeLapsedPlans(ctx, time.Now().UTC())
			if err != nil {
				o.log.Warn("plan_lapse_failed", "error", err.Error())
				continue
			}
			if n > 0 {
				o.log.Info("plan_lapse", "downgraded", n)
			}
		}
	}
}
