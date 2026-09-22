-- v0.35 audiences: named groups of existing contacts (membership only, no
-- sending). Many-to-many via audience_members. Tenant integrity is enforced
-- by composite FKs (tenant_id, audience_id)/(tenant_id, contact_id), not just
-- application checks: a membership row physically cannot reference an
-- audience/contact belonging to a different tenant than the row itself.
CREATE TABLE audiences (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name        TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name),
    -- Composite-FK target for audience_members below.
    UNIQUE (tenant_id, id)
);

CREATE INDEX idx_audiences_tenant_created ON audiences (tenant_id, created_at DESC, id DESC);

-- contacts needs the same (tenant_id, id) unique target; id alone is already
-- globally unique (PK), so this adds no new uniqueness guarantee, only a
-- composite-FK anchor.
ALTER TABLE contacts ADD CONSTRAINT uq_contacts_tenant_id UNIQUE (tenant_id, id);

-- audience_members carries tenant_id (denormalized from both sides) so the
-- two composite FKs below can require audience and contact to agree with the
-- row's own tenant AND with each other, transitively: PostgreSQL will reject
-- ANY insert where audience_id and contact_id do not both belong to
-- tenant_id, making cross-tenant membership impossible at the schema level,
-- not merely checked in Go.
CREATE TABLE audience_members (
    audience_id  TEXT NOT NULL,
    contact_id   TEXT NOT NULL,
    tenant_id    TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (audience_id, contact_id),
    FOREIGN KEY (tenant_id, audience_id) REFERENCES audiences(tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, contact_id) REFERENCES contacts(tenant_id, id) ON DELETE CASCADE
);

-- Query: "list members of an audience, oldest first" (keyset pagination).
-- The PK (audience_id, contact_id) already serves membership existence
-- checks and the audience-side cascade; this adds created_at ordering.
CREATE INDEX idx_audience_members_audience_created ON audience_members (audience_id, created_at, contact_id);

-- FK columns are not auto-indexed by Postgres: without this, every contact
-- delete's cascade into audience_members is a sequential scan.
CREATE INDEX idx_audience_members_contact ON audience_members (contact_id);

-- New API-key scopes: audiences:read/write.
ALTER TABLE api_keys DROP CONSTRAINT api_keys_scopes_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scopes_check CHECK (
    scopes <@ ARRAY[
        'emails:send','emails:read','domains:read','domains:write',
        'webhooks:read','webhooks:write','suppressions:read','suppressions:write',
        'templates:read','templates:write','contacts:read','contacts:write',
        'audiences:read','audiences:write'
    ]::text[]
);
