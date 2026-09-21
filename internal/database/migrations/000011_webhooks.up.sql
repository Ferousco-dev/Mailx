-- v0.22 webhook subscriptions and durable delivery pipeline. Existing events
-- remain the only lifecycle source of truth; these tables only track fan-out
-- and attempts to notify tenant-owned endpoints.

ALTER TABLE api_keys DROP CONSTRAINT api_keys_scopes_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scopes_check CHECK (
    scopes <@ ARRAY[
        'emails:send','emails:read','domains:read','domains:write',
        'webhooks:read','webhooks:write'
    ]::text[]
);

CREATE TABLE webhook_subscriptions (
    id                TEXT PRIMARY KEY,
    tenant_id         TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    url               TEXT NOT NULL,
    secret_ciphertext BYTEA NOT NULL,
    secret_nonce      BYTEA NOT NULL,
    event_types       TEXT[] NOT NULL CHECK (
        cardinality(event_types) > 0 AND
        event_types <@ ARRAY[
            'email.queued','email.delivered','email.delivery_delayed',
            'email.failed','email.bounced'
        ]::text[]
    ),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at       TIMESTAMPTZ,
    UNIQUE (tenant_id, id)
);

CREATE INDEX idx_webhook_subscriptions_tenant_created
    ON webhook_subscriptions (tenant_id, created_at DESC, id DESC)
    WHERE disabled_at IS NULL;

ALTER TABLE events ADD COLUMN fanned_out_at TIMESTAMPTZ;
CREATE UNIQUE INDEX uq_events_tenant_id ON events (tenant_id, id);
CREATE INDEX idx_events_pending_fanout
    ON events (occurred_at, id) WHERE fanned_out_at IS NULL;

CREATE TABLE webhook_deliveries (
    id                  TEXT PRIMARY KEY,
    tenant_id           TEXT NOT NULL,
    subscription_id     TEXT NOT NULL,
    event_id            TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'pending' CHECK (
        status IN ('pending','delivering','succeeded','failed','cancelled')
    ),
    attempt_count       INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_token         TEXT,
    lease_expires_at    TIMESTAMPTZ,
    last_error_category TEXT,
    last_response_code  INTEGER,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at        TIMESTAMPTZ,
    failed_at           TIMESTAMPTZ,
    FOREIGN KEY (tenant_id, subscription_id)
        REFERENCES webhook_subscriptions(tenant_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, event_id)
        REFERENCES events(tenant_id, id) ON DELETE CASCADE,
    UNIQUE (subscription_id, event_id),
    CHECK ((status = 'delivering') = (lease_token IS NOT NULL AND lease_expires_at IS NOT NULL))
);

CREATE INDEX idx_webhook_deliveries_pending
    ON webhook_deliveries (next_attempt_at, created_at, id)
    WHERE status = 'pending';
CREATE INDEX idx_webhook_deliveries_expired_lease
    ON webhook_deliveries (lease_expires_at, id)
    WHERE status = 'delivering';
CREATE INDEX idx_webhook_deliveries_subscription_created
    ON webhook_deliveries (tenant_id, subscription_id, created_at DESC, id DESC);

CREATE TABLE webhook_delivery_attempts (
    id               TEXT PRIMARY KEY,
    delivery_id      TEXT NOT NULL REFERENCES webhook_deliveries(id) ON DELETE CASCADE,
    attempt_number   INTEGER NOT NULL CHECK (attempt_number > 0),
    status           TEXT NOT NULL DEFAULT 'in_progress' CHECK (
        status IN ('in_progress','succeeded','retrying','failed')
    ),
    attempted_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ,
    duration_ms      BIGINT CHECK (duration_ms IS NULL OR duration_ms >= 0),
    response_code    INTEGER,
    error_category   TEXT,
    response_excerpt TEXT CHECK (octet_length(response_excerpt) <= 4096),
    next_retry_at    TIMESTAMPTZ,
    UNIQUE (delivery_id, attempt_number)
);
