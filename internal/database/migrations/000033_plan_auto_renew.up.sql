-- v0.47 phase 3c: opt-in plan auto-renewal (RSK-044).
-- auto_renew is OFF by default and only an org owner can turn it on.
-- The Paystack authorization code (a reusable card token) is stored ONLY
-- encrypted (secretbox, AES-256-GCM under MAILX_BILLING_MASTER_KEY, bound to
-- the tenant id as associated data); plaintext never touches the database.
-- plan_reminder_period_end/_sent_at/_auto_renew record the one reminder sent
-- for the current period (once per period; also the "owner was warned of
-- the charge" precondition for any auto-renewal charge).
ALTER TABLE tenants
    ADD COLUMN auto_renew BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN paystack_auth_ciphertext BYTEA,
    ADD COLUMN paystack_auth_nonce BYTEA,
    ADD COLUMN paystack_auth_email TEXT,
    ADD COLUMN plan_reminder_period_end TIMESTAMPTZ,
    ADD COLUMN plan_reminder_sent_at TIMESTAMPTZ,
    ADD COLUMN plan_reminder_auto_renew BOOLEAN NOT NULL DEFAULT false;

-- One row per MailX-initiated renewal charge attempt (audit log + the claim
-- that guarantees at most one in-flight charge per tenant period).
-- reference is deterministic per (tenant, period_end, attempt) and is sent to
-- Paystack as the transaction reference, so it is also the billing_payments
-- key: the webhook and the ticker applying the same charge collapse to one.
CREATE TABLE billing_renewal_attempts (
    tenant_id      TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    period_end     TIMESTAMPTZ NOT NULL,
    attempt        SMALLINT NOT NULL CHECK (attempt BETWEEN 1 AND 3),
    reference      TEXT NOT NULL UNIQUE,
    plan           TEXT NOT NULL CHECK (plan IN ('plus', 'pro')),
    amount         BIGINT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'succeeded', 'failed')),
    failure_reason TEXT,
    created_at     TIMESTAMPTZ NOT NULL,
    finished_at    TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, period_end, attempt)
);
CREATE INDEX idx_billing_renewal_attempts_pending ON billing_renewal_attempts (created_at) WHERE status = 'pending';
