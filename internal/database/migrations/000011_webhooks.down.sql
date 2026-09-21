DROP TABLE IF EXISTS webhook_delivery_attempts;
DROP TABLE IF EXISTS webhook_deliveries;
DROP INDEX IF EXISTS idx_events_pending_fanout;
DROP INDEX IF EXISTS uq_events_tenant_id;
ALTER TABLE events DROP COLUMN IF EXISTS fanned_out_at;
DROP TABLE IF EXISTS webhook_subscriptions;

-- Keep the expanded API-key scope CHECK on rollback. Historical credentials
-- may already contain webhook scopes, and older binaries ignore unused scopes.
