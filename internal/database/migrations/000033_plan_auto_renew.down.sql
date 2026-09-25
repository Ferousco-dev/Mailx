DROP TABLE IF EXISTS billing_renewal_attempts;
ALTER TABLE tenants
    DROP COLUMN IF EXISTS plan_reminder_auto_renew,
    DROP COLUMN IF EXISTS plan_reminder_sent_at,
    DROP COLUMN IF EXISTS plan_reminder_period_end,
    DROP COLUMN IF EXISTS paystack_auth_email,
    DROP COLUMN IF EXISTS paystack_auth_nonce,
    DROP COLUMN IF EXISTS paystack_auth_ciphertext,
    DROP COLUMN IF EXISTS auto_renew;
