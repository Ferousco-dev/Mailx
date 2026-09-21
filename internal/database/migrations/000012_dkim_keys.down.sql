DROP TABLE IF EXISTS dkim_keys;
ALTER TABLE domains DROP CONSTRAINT IF EXISTS uq_domains_tenant_id;
