-- v0.31 fair dispatch. The dispatcher used to read pending outbox rows in global
-- available_at order (idx_outbox_pending), so one tenant with a large backlog filled
-- every batch and delayed everyone behind it (head-of-line starvation). Fair dispatch
-- finds each tenant that has due work (a loose index scan) and takes each tenant's
-- oldest rows, which needs (tenant_id, available_at) over the same "pending" rows.
--
-- idx_outbox_pending is DROPPED, not kept: its only reader was the old global-order
-- query, and while it exists the planner prefers it for the per-tenant probe and then
-- filters by tenant (measured: 15000 rows read and discarded behind one large backlog;
-- with only the tenant index the probe reads exactly the tenant's oldest rows). One
-- index instead of two also halves outbox write cost and index memory. Dispatched
-- rows never enter either index.
CREATE INDEX idx_outbox_pending_tenant ON outbox (tenant_id, available_at) WHERE dispatched_at IS NULL;
DROP INDEX IF EXISTS idx_outbox_pending;
