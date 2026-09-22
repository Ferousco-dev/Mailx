-- v0.33 reusable email templates: tenant-owned content, rendered into an
-- explicit subject/text/html before the existing MIME builder ever runs (no
-- second delivery pipeline). Durable config, not delivery telemetry: not
-- retention-bound, lives until the tenant deletes it (see docs/design-v0.33.md).
CREATE TABLE templates (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name        TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    subject     TEXT NOT NULL CHECK (length(subject) <= 500),
    text_body   TEXT NOT NULL DEFAULT '' CHECK (length(text_body) <= 2097152),
    html_body   TEXT NOT NULL DEFAULT '' CHECK (length(html_body) <= 2097152),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (length(text_body) > 0 OR length(html_body) > 0),
    -- Unique per tenant: names are how a developer picks a template out of a
    -- list, so two rows with the same name is a real DX footgun, not merely
    -- a display concern — enforced in the database, not only in the API.
    UNIQUE (tenant_id, name)
);

-- Query: "list this tenant's templates, newest first" (keyset pagination,
-- mirrors idx_suppressions_tenant_created/idx_domains_tenant_created).
CREATE INDEX idx_templates_tenant_created ON templates (tenant_id, created_at DESC, id DESC);

-- New API-key scopes: templates:read/write.
ALTER TABLE api_keys DROP CONSTRAINT api_keys_scopes_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scopes_check CHECK (
    scopes <@ ARRAY[
        'emails:send','emails:read','domains:read','domains:write',
        'webhooks:read','webhooks:write','suppressions:read','suppressions:write',
        'templates:read','templates:write'
    ]::text[]
);
