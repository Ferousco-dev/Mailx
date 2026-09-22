-- v0.34 contacts: "this tenant knows this recipient" — independent of
-- suppressions ("MailX must not currently send here") and independent of
-- message recipient rows (SMTP transport state). No FK from suppressions or
-- recipients to contacts, and no FK from contacts to them: deleting/recreating
-- a contact must never touch suppression or delivery history.
CREATE TABLE contacts (
    id                TEXT PRIMARY KEY,
    tenant_id         TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    -- contact.Normalize's key: ASCII, domain lower-cased, local part CASE
    -- PRESERVED (unlike suppressions.email — see internal/contact's doc for why
    -- these two normalization policies must differ).
    normalized_email  TEXT NOT NULL CHECK (length(normalized_email) BETWEEN 3 AND 254 AND normalized_email !~ '\s'),
    email             TEXT NOT NULL, -- as submitted, for display
    name              TEXT NOT NULL DEFAULT '' CHECK (length(name) <= 200),
    attributes        JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(attributes) = 'object' AND length(attributes::text) <= 8192),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, normalized_email)
);

-- Query: "list this tenant's contacts, newest first" (keyset pagination,
-- mirrors idx_templates_tenant_created/idx_suppressions_tenant_created).
CREATE INDEX idx_contacts_tenant_created ON contacts (tenant_id, created_at DESC, id DESC);

-- New API-key scopes: contacts:read/write.
ALTER TABLE api_keys DROP CONSTRAINT api_keys_scopes_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scopes_check CHECK (
    scopes <@ ARRAY[
        'emails:send','emails:read','domains:read','domains:write',
        'webhooks:read','webhooks:write','suppressions:read','suppressions:write',
        'templates:read','templates:write','contacts:read','contacts:write'
    ]::text[]
);
