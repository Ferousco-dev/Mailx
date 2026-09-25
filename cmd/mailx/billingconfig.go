package main

import (
	"context"
	"os"
	"time"

	"github.com/Ferousco-dev/mailx/internal/api"
	"github.com/Ferousco-dev/mailx/internal/billing"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/secretbox"
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
	cfg := &api.BillingConfig{Paystack: ps, CallbackURL: os.Getenv("MAILX_PAYSTACK_CALLBACK_URL")}
	// MAILX_BILLING_MASTER_KEY (base64, 32 bytes; its own key, never shared
	// with the DKIM/webhook keys) encrypts saved card authorizations. Unset:
	// auto-renewal is unavailable (reminders still go out); set but invalid:
	// startup fails rather than silently disabling a money path.
	if enc := os.Getenv("MAILX_BILLING_MASTER_KEY"); enc != "" {
		key, err := secretbox.DecodeKey(enc, "MAILX_BILLING_MASTER_KEY")
		if err != nil {
			return nil, err
		}
		box, err := secretbox.New(key)
		if err != nil {
			return nil, err
		}
		cfg.AuthBox = box
	}
	db.EnablePlanEnforcement()
	return cfg, nil
}

// enablePlanEnforcementFromEnv is the admin-CLI equivalent (no routes): it
// keeps `mailx purge-retention`/`show-retention` consistent with the server.
func enablePlanEnforcementFromEnv(db *database.DB) {
	if os.Getenv("MAILX_PAYSTACK_SECRET_KEY") != "" {
		db.EnablePlanEnforcement()
	}
}

// planLapseInterval is how often the billing pass (reminders, auto-renewal
// charges, then lapsing) runs.
const planLapseInterval = time.Hour

// runPlanLapse runs the hourly billing pass: renewer (reminders before every
// period end; opt-in auto-renewal charges, DEC-228..231) then lapse of any
// paid tenant whose period has ended (retention pinned, DEC-227). renewer may
// be nil (no system mailer): then nothing is reminded or charged.
func runPlanLapse(ctx context.Context, db *database.DB, renewer *api.Renewer, o obs) error {
	ticker := time.NewTicker(planLapseInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if renewer != nil {
				renewer.RunOnce(ctx, time.Now().UTC())
			}
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
