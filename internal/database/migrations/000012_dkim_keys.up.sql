-- v0.26 DKIM signing keys. A key belongs to exactly one MailX domain row (and
-- through it one tenant). The private key is stored ONLY as AES-GCM ciphertext
-- (nonce alongside, associated data binds it to tenant/domain/selector); the
-- database never holds a plaintext private key. Public material is stored to
-- render DNS instructions and to verify publication.
--
-- Lifecycle: pending (generated, not yet published) -> active (published and
-- verified; the only signing key) -> retired (never signs again; its private
-- ciphertext is destroyed at retirement). Domain deletion deletes the keys.

-- Composite key target so dkim_keys can prove tenant consistency with domains.
ALTER TABLE domains ADD CONSTRAINT uq_domains_tenant_id UNIQUE (tenant_id, id);

CREATE TABLE dkim_keys (
    id                 TEXT PRIMARY KEY,
    tenant_id          TEXT NOT NULL,
    domain_id          TEXT NOT NULL,
    selector           TEXT NOT NULL
        CHECK (selector ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    algorithm          TEXT NOT NULL CHECK (algorithm = 'rsa-sha256'),
    key_bits           INTEGER NOT NULL CHECK (key_bits >= 2048),
    public_key         TEXT NOT NULL CHECK (public_key <> ''),
    private_ciphertext BYTEA,
    private_nonce      BYTEA,
    status             TEXT NOT NULL CHECK (status IN ('pending', 'active', 'retired')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    activated_at       TIMESTAMPTZ,
    retired_at         TIMESTAMPTZ,
    FOREIGN KEY (tenant_id, domain_id) REFERENCES domains (tenant_id, id) ON DELETE CASCADE,
    UNIQUE (domain_id, selector),
    -- Only retired keys have no private material; every other key must.
    CHECK ((status = 'retired') = (private_ciphertext IS NULL AND private_nonce IS NULL)),
    CHECK (status <> 'pending' OR (activated_at IS NULL AND retired_at IS NULL)),
    CHECK (status <> 'active'  OR (activated_at IS NOT NULL AND retired_at IS NULL)),
    CHECK (status <> 'retired' OR retired_at IS NOT NULL)
);

-- At most one signing key and at most one key awaiting publication per domain.
-- These partial unique indexes are the concurrency guarantee: two racing
-- rotations or activations cannot both succeed.
--   Query served: "the active key for domain D" (signing hot path, after the
--   domains lookup by (tenant_id, name) via uq_domains_tenant_active_name).
--   Cost: tiny (at most one row per domain per index).
CREATE UNIQUE INDEX uq_dkim_one_active  ON dkim_keys (domain_id) WHERE status = 'active';
CREATE UNIQUE INDEX uq_dkim_one_pending ON dkim_keys (domain_id) WHERE status = 'pending';
