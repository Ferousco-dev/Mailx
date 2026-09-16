CREATE TABLE domains (
    id                  TEXT PRIMARY KEY,
    tenant_id           TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name                TEXT NOT NULL,
    verification_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (verification_status IN ('pending', 'verified')),
    verification_token  TEXT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    verified_at         TIMESTAMPTZ,
    last_checked_at     TIMESTAMPTZ,
    deleted_at          TIMESTAMPTZ,
    CHECK ((verification_status = 'verified') = (verified_at IS NOT NULL))
);

-- v0.19 intentionally enforces its fixed scope vocabulary in PostgreSQL as
-- well as Go. Extend that vocabulary in the same migration that introduces
-- the first domain routes.
ALTER TABLE api_keys DROP CONSTRAINT api_keys_scopes_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scopes_check
    CHECK (scopes <@ ARRAY['emails:send','emails:read','domains:read','domains:write']::text[]);

-- One active resource per canonical domain per tenant. Pending claims by
-- different tenants remain possible, so an unverified squatter cannot block
-- the real DNS controller from proving ownership.
CREATE UNIQUE INDEX uq_domains_tenant_active_name
    ON domains (tenant_id, name)
    WHERE deleted_at IS NULL;

-- DNS proof is authoritative, but only one tenant may hold active verified
-- ownership of a canonical name at a time.
CREATE UNIQUE INDEX uq_domains_active_verified_name
    ON domains (name)
    WHERE deleted_at IS NULL AND verification_status = 'verified';

-- Keyset list query: one tenant's active domains, newest first.
CREATE INDEX idx_domains_tenant_created
    ON domains (tenant_id, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;
