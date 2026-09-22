-- v0.36 broadcasts: durable bulk-send orchestration over the EXISTING
-- send pipeline (messages/outbox/queue/worker/DKIM/SMTP) — never a second
-- delivery engine. See docs/design-v0.36.md.
--
-- audience_id/template_id/contact_id carry NO live FK: both audience
-- snapshot (which contacts) and template snapshot (subject/text/html) are
-- COPIED at acceptance/snapshot time, so a broadcast is self-contained and
-- survives the source Audience/Template/Contact being edited or deleted
-- afterward. This is deliberate (see "snapshot semantics" in the design
-- doc), not an oversight.
CREATE TABLE broadcasts (
    id                          TEXT PRIMARY KEY,
    tenant_id                   TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    audience_id                 TEXT NOT NULL, -- provenance only; see doc above
    template_id                 TEXT NOT NULL, -- provenance only; see doc above
    name                        TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    from_address                TEXT NOT NULL,
    reply_to                    TEXT NOT NULL DEFAULT '',
    -- Template SOURCE snapshot (still contains {{tokens}}): rendering with
    -- per-recipient variables happens once per recipient at materialization
    -- time, from THIS frozen copy, never from the live templates table.
    subject_template            TEXT NOT NULL CHECK (length(subject_template) <= 500),
    text_template                TEXT NOT NULL DEFAULT '',
    html_template                TEXT NOT NULL DEFAULT '',
    variables                   JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(variables) = 'object' AND length(variables::text) <= 8192),
    status                      TEXT NOT NULL DEFAULT 'accepted' CHECK (status IN ('accepted','expanding','completed','failed')),
    failure_reason              TEXT,
    -- The point-in-time snapshot boundary: expansion only ever includes
    -- audience_members rows with created_at <= this instant, so a contact
    -- added to the Audience AFTER acceptance can never be included, however
    -- long expansion takes. A member REMOVED during expansion, before its
    -- batch is scanned, is a documented narrow exception (RSK, see design doc).
    audience_snapshot_at        TIMESTAMPTZ NOT NULL,
    snapshot_complete           BOOLEAN NOT NULL DEFAULT false,
    -- Durable resumable-scan checkpoint into audience_members, advanced one
    -- bounded batch at a time by the expansion poller; crash-safe because
    -- broadcast_recipients' own UNIQUE(broadcast_id, contact_id) makes
    -- re-scanning the same range after a crash idempotent regardless of
    -- exactly where the cursor was last durably advanced.
    expansion_cursor_created_at TIMESTAMPTZ,
    expansion_cursor_contact_id TEXT,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at                  TIMESTAMPTZ,
    completed_at                TIMESTAMPTZ,
    -- Composite-FK target for broadcast_recipients below.
    UNIQUE (tenant_id, id)
);

CREATE INDEX idx_broadcasts_tenant_created ON broadcasts (tenant_id, created_at DESC, id DESC);

-- Query: "which broadcasts still need expansion work" (the poller's only
-- scan). Partial index keeps it small forever: completed/failed broadcasts
-- (the overwhelming majority over time) never appear in it — same pattern
-- as idx_outbox_pending.
CREATE INDEX idx_broadcasts_active ON broadcasts (created_at) WHERE status IN ('accepted','expanding');

-- broadcast_recipients: one durable row per (broadcast, contact) — the
-- snapshot AND the materialization-progress record in one place. id doubles
-- as the eventual messages.id once materialized (see design doc "internal
-- materialization idempotency"): the SAME id is never generated twice for
-- the same (broadcast,contact) because of the UNIQUE constraint below, so a
-- retried materialization attempt either creates the message once or finds
-- it already exists and treats that as success.
CREATE TABLE broadcast_recipients (
    id           TEXT PRIMARY KEY,
    broadcast_id TEXT NOT NULL,
    tenant_id    TEXT NOT NULL,
    contact_id   TEXT NOT NULL, -- provenance only; no FK, survives contact deletion
    email        TEXT NOT NULL, -- snapshot at scan time, never re-read from contacts
    name         TEXT NOT NULL DEFAULT '',
    attributes   JSONB NOT NULL DEFAULT '{}'::jsonb,
    status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','suppressed','materialized')),
    message_id   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (broadcast_id, contact_id),
    FOREIGN KEY (tenant_id, broadcast_id) REFERENCES broadcasts (tenant_id, id) ON DELETE CASCADE
);

-- Query: "list recipients of a broadcast, oldest first" (keyset pagination).
CREATE INDEX idx_broadcast_recipients_broadcast_created ON broadcast_recipients (broadcast_id, created_at, id);

-- Query: "next batch of pending recipients to materialize, oldest first"
-- (the poller's other hot query). created_at is included so pending rows
-- can be found in order WITHOUT a sort or a scan through already-processed
-- rows: measured at 50k rows/98% processed, a (broadcast_id)-only partial
-- index still let the planner choose idx_broadcast_recipients_broadcast_created
-- and filter ~43k non-pending rows (worst case: remaining pending rows
-- cluster at the tail of a nearly-finished broadcast); WITH created_at here
-- the same query drops to an index-only scan of just the pending rows.
CREATE INDEX idx_broadcast_recipients_pending ON broadcast_recipients (broadcast_id, created_at) WHERE status = 'pending';

ALTER TABLE api_keys DROP CONSTRAINT api_keys_scopes_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scopes_check CHECK (
    scopes <@ ARRAY[
        'emails:send','emails:read','domains:read','domains:write',
        'webhooks:read','webhooks:write','suppressions:read','suppressions:write',
        'templates:read','templates:write','contacts:read','contacts:write',
        'audiences:read','audiences:write','broadcasts:read','broadcasts:write'
    ]::text[]
);
