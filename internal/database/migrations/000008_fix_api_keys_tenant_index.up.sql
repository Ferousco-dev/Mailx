-- 000006's idx_api_keys_tenant_active (WHERE revoked_at IS NULL) cannot
-- serve ListAPIKeysForTenant, which deliberately returns active AND
-- historical keys with no revoked_at predicate (see that function's own
-- doc) - a partial index never matches a query missing its exact
-- predicate. Replace it with a plain index actually covering the one
-- query this table serves besides the key_id lookup. Never edit an
-- already-applied migration (000006) - fix forward instead.
DROP INDEX idx_api_keys_tenant_active;
CREATE INDEX idx_api_keys_tenant_created ON api_keys (tenant_id, created_at DESC);
