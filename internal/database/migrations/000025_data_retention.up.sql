-- v0.46 per-tenant data retention. NULL means "use the platform default"
-- (DefaultRetentionDays in internal/database/retention.go) rather than
-- "retain forever" - an unconfigured tenant still gets purged on the same
-- schedule as everyone else, which is the safer default for a compliance
-- feature: an operator who never touches this setting should not
-- accidentally retain PII indefinitely.
ALTER TABLE tenants ADD COLUMN retention_days INTEGER
    CHECK (retention_days IS NULL OR retention_days > 0);
