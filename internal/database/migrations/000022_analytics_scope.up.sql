-- v0.38 analytics: read-only over existing durable facts. No new table —
-- internal/database/analytics.go aggregates directly from the existing
-- `events` table (tenant_id, event_type, occurred_at already indexed via
-- idx_events_tenant_occurred), which already records exactly the lifecycle
-- facts analytics needs (queued/delivered/deferred/bounced/failed/
-- suppressed/complained), each historically timestamped and never mutually
-- exclusive with an earlier fact for the same message. See docs/design-v0.38
-- notes in .ilana/architecture.md for the full metric vocabulary.
ALTER TABLE api_keys DROP CONSTRAINT api_keys_scopes_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scopes_check CHECK (
    scopes <@ ARRAY[
        'emails:send','emails:read','domains:read','domains:write',
        'webhooks:read','webhooks:write','suppressions:read','suppressions:write',
        'templates:read','templates:write','contacts:read','contacts:write',
        'audiences:read','audiences:write','broadcasts:read','broadcasts:write',
        'analytics:read'
    ]::text[]
);
