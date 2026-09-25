DROP TABLE IF EXISTS billing_payments;
DROP INDEX IF EXISTS idx_tenants_paid_period_end;
ALTER TABLE tenants
    DROP COLUMN IF EXISTS paystack_customer_code,
    DROP COLUMN IF EXISTS plan_current_period_end,
    DROP COLUMN IF EXISTS plan_status,
    DROP COLUMN IF EXISTS plan;
