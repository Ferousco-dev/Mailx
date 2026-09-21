-- v0.30 recipient suppression.
--
-- A suppression is durable POLICY: this tenant must not send ordinary mail to this
-- address. It is not a delivery fact: message and attempt history are never
-- rewritten by creating or deleting one. Tenant-scoped (a bounce in one tenant
-- never influences another). PostgreSQL is authoritative; there is no cache.
CREATE TABLE suppressions (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    -- The canonical key produced by internal/suppression.Normalize: ASCII,
    -- lower-cased, one '@'. The CHECK is a backstop, not the normalizer.
    email           TEXT NOT NULL CHECK (
                        email = lower(email) AND email !~ '\s' AND length(email) BETWEEN 3 AND 254
                        AND length(email) - length(replace(email, '@', '')) = 1),
    -- WHY. complaint and unsubscribe are reserved vocabulary: nothing produces
    -- them yet (v0.32 feedback loops, a later subscription product), so adding
    -- them later needs no schema change.
    reason          TEXT NOT NULL CHECK (reason IN ('manual', 'hard_bounce', 'complaint', 'unsubscribe')),
    -- HOW it was created, independent of the reason.
    source          TEXT NOT NULL CHECK (source IN ('api', 'delivery', 'feedback')),
    -- Evidence for hard_bounce: the message and the structured SMTP verdict.
    message_id      TEXT REFERENCES messages(id) ON DELETE SET NULL,
    smtp_code       INTEGER CHECK (smtp_code IS NULL OR smtp_code BETWEEN 100 AND 599),
    enhanced_status TEXT CHECK (enhanced_status IS NULL OR enhanced_status ~ '^[245]\.[0-9]{1,3}\.[0-9]{1,3}$'),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- One effective suppression per tenant and address. This constraint is the
    -- concurrency control: any number of workers or requests racing to create the
    -- same suppression converge on one row (INSERT ... ON CONFLICT DO NOTHING).
    -- Its index is also the hot delivery-path lookup (tenant_id, email).
    CONSTRAINT uq_suppressions_tenant_email UNIQUE (tenant_id, email)
);

-- Keyset listing for GET /v1/suppressions: newest first per tenant, matching the
-- (created_at, id) cursor used by the other list endpoints.
CREATE INDEX idx_suppressions_tenant_created ON suppressions (tenant_id, created_at DESC, id DESC);

-- New API-key scopes.
ALTER TABLE api_keys DROP CONSTRAINT api_keys_scopes_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scopes_check CHECK (
    scopes <@ ARRAY[
        'emails:send','emails:read','domains:read','domains:write',
        'webhooks:read','webhooks:write','suppressions:read','suppressions:write'
    ]::text[]
);

-- A message whose every recipient was suppressed is terminal and is NOT a failure:
-- no SMTP attempt was made. A recipient skipped by suppression is 'suppressed',
-- not 'failed' or 'bounced'.
ALTER TABLE messages DROP CONSTRAINT messages_status_check;
ALTER TABLE messages ADD CONSTRAINT messages_status_check
    CHECK (status IN ('queued','processing','retrying','delivered','failed','bounced','suppressed'));
ALTER TABLE recipients DROP CONSTRAINT recipients_status_check;
ALTER TABLE recipients ADD CONSTRAINT recipients_status_check
    CHECK (status IN ('pending','delivered','failed','suppressed'));

-- Lifecycle event: recipients of a message were skipped by suppression. At most one
-- per message, so replays after a crash cannot duplicate it.
ALTER TABLE events DROP CONSTRAINT events_event_type_check;
ALTER TABLE events ADD CONSTRAINT events_event_type_check
    CHECK (event_type IN ('queued','delivery_attempted','delivered','deferred','bounced','failed','suppressed'));
CREATE UNIQUE INDEX uq_events_message_suppressed ON events (message_id) WHERE event_type = 'suppressed';

ALTER TABLE webhook_subscriptions DROP CONSTRAINT webhook_subscriptions_event_types_check;
ALTER TABLE webhook_subscriptions ADD CONSTRAINT webhook_subscriptions_event_types_check CHECK (
    cardinality(event_types) > 0 AND
    event_types <@ ARRAY[
        'email.queued','email.delivered','email.delivery_delayed',
        'email.failed','email.bounced','email.suppressed'
    ]::text[]
);
