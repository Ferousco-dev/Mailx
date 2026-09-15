-- Discovered via EXPLAIN during v0.15 development: idx_messages_tenant_status_created
-- (tenant_id, status, created_at DESC) does NOT serve "list messages for a
-- tenant, no status filter, ORDER BY created_at DESC LIMIT N" efficiently.
-- With status sitting between tenant_id and created_at, the index's row
-- order for a fixed tenant_id is grouped by status first — it does NOT
-- give a global created_at ordering across all statuses, so the planner
-- cannot use it to satisfy ORDER BY created_at DESC without an extra sort,
-- and falls back to a sequential scan once the table has enough rows to
-- make the index not obviously cheaper.
--
-- This is a distinct query SHAPE from the status-filtered case (which
-- idx_messages_tenant_status_created continues to serve well) — not a
-- redundant index. "List everything for this tenant, newest first" is
-- almost certainly the more common real access pattern (a general
-- inbox/sent view), so it gets its own index.
CREATE INDEX idx_messages_tenant_created
    ON messages (tenant_id, created_at DESC);
