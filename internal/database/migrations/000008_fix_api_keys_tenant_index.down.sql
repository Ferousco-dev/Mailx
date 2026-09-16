DROP INDEX idx_api_keys_tenant_created;
CREATE INDEX idx_api_keys_tenant_active ON api_keys (tenant_id) WHERE revoked_at IS NULL;
