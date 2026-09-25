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
