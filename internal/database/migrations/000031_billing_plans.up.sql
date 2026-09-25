-- v0.47 phase 2: billing plans (Free/Plus/Pro) and Paystack payments.
-- Every existing and new tenant defaults to 'free'. Plan LIMITS are only
-- enforced when the deployment configures MAILX_PAYSTACK_SECRET_KEY (see
-- DEC-221): a self-hosted instance keeps its pre-billing unlimited behavior
-- even though its tenants carry plan = 'free' here.
ALTER TABLE tenants
    ADD COLUMN plan TEXT NOT NULL DEFAULT 'free' CHECK (plan IN ('free', 'plus', 'pro')),
    ADD COLUMN plan_status TEXT NOT NULL DEFAULT 'active' CHECK (plan_status IN ('active', 'lapsed')),
    ADD COLUMN plan_current_period_end TIMESTAMPTZ,
    ADD COLUMN paystack_customer_code TEXT;

-- Pin every PRE-EXISTING tenant's effective retention window at today's
-- flat default (90 days, DefaultRetentionDays) by making it explicit. A
-- NULL retention_days now falls back to the tenant's PLAN's window when
-- enforcement is on (Free = 7 days) instead of the flat 90 - without this
-- backfill, an operator turning on MAILX_PAYSTACK_SECRET_KEY for the first
-- time on an existing deployment would silently shrink every tenant's
-- retention window from 90 to 7 days, and the next hourly retention-purge
-- run would irreversibly hard-delete any terminal message between 7 and 90
-- days old (data-loss finding, PR #24 review). A brand-new tenant created
-- after billing is enabled has no messages yet, so the plan's own window
-- applying to it from day one is correct, not a regression.
UPDATE tenants SET retention_days = 90 WHERE retention_days IS NULL;

-- The lapse ticker scans only paid tenants.
CREATE INDEX idx_tenants_paid_period_end ON tenants (plan_current_period_end) WHERE plan <> 'free';

-- One row per Paystack transaction reference ever applied. The primary key
-- makes webhook application idempotent: a replayed (still validly signed)
-- charge.success can never extend a plan a second time.
CREATE TABLE billing_payments (
    reference   TEXT PRIMARY KEY CHECK (length(reference) BETWEEN 1 AND 200),
    tenant_id   TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    plan        TEXT NOT NULL CHECK (plan IN ('plus', 'pro')),
    amount      BIGINT NOT NULL,
    currency    TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_billing_payments_tenant ON billing_payments (tenant_id, created_at DESC);
