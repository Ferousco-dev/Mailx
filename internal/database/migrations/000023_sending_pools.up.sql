-- v0.39 Sending Pools / Routing: minimal durable routing schema.
--
-- Three distinct layers, kept intentionally separate:
--   1. Configured routing policy   -> sending_pools, sending_pool_members
--      (operator-managed, mutable at any time).
--   2. Durable message routing decision -> messages.sending_member_id
--      (set once at acceptance by SelectRoute, immutable afterwards by
--      application convention; retry stickiness always reads this, never
--      re-selects). Deliberately NOT paired with a redundant pool_id: the
--      pool is only a selection-time input, the member is the decision.
--   3. Actual infrastructure used for one delivery attempt ->
--      delivery_attempts.{transport_kind,effective_hostname,
--      effective_source_ip}. These are historical facts snapshotted at
--      attempt time, denormalized ON PURPOSE (no FK) so they remain true
--      even if the member/pool config later changes. sending_member_id is
--      also copied onto delivery_attempts for correlation/joins, but the
--      snapshot columns -- not that id -- are what answer "what actually
--      sent this" once config has moved on. No secrets are ever stored
--      here; relay credentials stay environment-managed
--      (MAILX_RELAY_*), never per-member.
--
-- enabled=false (pool or member) is a real operator kill switch: it blocks
-- future member selection AND blocks any new SMTP attempt through that
-- infrastructure for messages already routed to it. It never reroutes or
-- rotates a message to a different member -- that would be exactly the
-- receiver-policy-evasion behavior v0.39 must not introduce. A message
-- whose member is disabled is held (existing outbox/claim release
-- mechanism, zero SMTP attempts, zero attempt records) and resumes through
-- the SAME member once re-enabled.
--
-- Zero pools configured, or a domain with no sending_pool_id, preserves
-- current behavior exactly: sending_member_id stays NULL end-to-end and the
-- single pre-v0.39 Engine/Client path is unchanged.
--
-- domains.sending_pool_id is operator-controlled infrastructure policy, not
-- an ordinary tenant-writable domain field -- see internal/api/domain
-- handlers for the enforcement boundary (this migration only creates the
-- column; it does not by itself expose it to tenants).
--
-- No pool/member deletion in v0.39 (disable only), no weighted routing, no
-- per-member relay credentials, no automatic failover.

CREATE TABLE sending_pools (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    enabled    BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_sending_pools_name UNIQUE (name)
);

CREATE TABLE sending_pool_members (
    id         TEXT PRIMARY KEY,
    pool_id    TEXT NOT NULL REFERENCES sending_pools(id) ON DELETE RESTRICT,
    kind       TEXT NOT NULL,
    hostname   TEXT,
    source_ip  INET,
    enabled    BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT sending_pool_members_kind_check CHECK (kind IN ('direct', 'relay')),
    -- A relay member carries no per-member identity: it means "use the
    -- process's single existing MAILX_RELAY_* config", so hostname/
    -- source_ip (direct-delivery dial config) must stay unset for it.
    CONSTRAINT sending_pool_members_relay_no_identity_check CHECK (
        kind <> 'relay' OR (hostname IS NULL AND source_ip IS NULL)
    )
);

CREATE INDEX idx_sending_pool_members_pool ON sending_pool_members (pool_id);

ALTER TABLE domains ADD COLUMN sending_pool_id TEXT REFERENCES sending_pools(id) ON DELETE SET NULL;

ALTER TABLE messages ADD COLUMN sending_member_id TEXT REFERENCES sending_pool_members(id) ON DELETE RESTRICT;

ALTER TABLE delivery_attempts ADD COLUMN sending_member_id TEXT;
ALTER TABLE delivery_attempts ADD COLUMN transport_kind TEXT;
ALTER TABLE delivery_attempts ADD COLUMN effective_hostname TEXT;
ALTER TABLE delivery_attempts ADD COLUMN effective_source_ip INET;
ALTER TABLE delivery_attempts ADD CONSTRAINT delivery_attempts_transport_kind_check
    CHECK (transport_kind IS NULL OR transport_kind IN ('direct', 'relay'));
