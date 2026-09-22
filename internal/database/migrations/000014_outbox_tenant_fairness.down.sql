CREATE INDEX IF NOT EXISTS idx_outbox_pending ON outbox (available_at) WHERE dispatched_at IS NULL;
DROP INDEX IF EXISTS idx_outbox_pending_tenant;
