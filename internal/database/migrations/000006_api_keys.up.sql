-- v0.19: application/developer API-key authentication. One row per key;
-- the raw secret is NEVER stored — only key_id (a safe, non-secret lookup
-- identifier) and secret_hash (HMAC-SHA256 of the secret, see
-- internal/auth's verifier doc for the exact threat model). Rows are
-- never deleted, even when revoked/rotated away — see revoked_at/
-- replaced_by_id below - so incident review and future dashboard display
-- always have history to show.
CREATE TABLE api_keys (
    id             TEXT PRIMARY KEY,
    tenant_id      TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    name           TEXT NOT NULL,
    key_id         TEXT NOT NULL,
    secret_hash    TEXT NOT NULL,
    -- Known-scope-only, enforced at the database level too (not just in
    -- application code) so a bug can never persist an unrecognized scope.
    scopes         TEXT[] NOT NULL CHECK (scopes <@ ARRAY['emails:send','emails:read']::text[]),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at   TIMESTAMPTZ,
    -- NULL = no expiration. Also how rotation's grace/overlap window is
    -- represented: rotating with a grace period sets the OLD key's
    -- expires_at to now()+grace rather than inventing a second
    -- "retiring_at" concept with the same "becomes invalid at T" meaning.
    expires_at     TIMESTAMPTZ,
    -- Immediate, durable invalidation, independent of expires_at. A
    -- revoked key is rejected regardless of what expires_at says.
    revoked_at     TIMESTAMPTZ,
    -- Set on the OLD key by a rotation that created this new row; purely
    -- informational/audit (authentication never reads it) - NOT a FK
    -- constraint, since the "new" side does not exist yet at the moment
    -- the old row would otherwise need to reference it, and rows are
    -- never deleted so there is no dangling-reference risk to guard.
    replaced_by_id TEXT
);

-- Query: "locate the one candidate row for an incoming credential" — the
-- only query authentication ever runs, on every single API request. Must
-- be UNIQUE (key_id is meant to identify at most one row) and must be the
-- single index that actually matters for request latency.
CREATE UNIQUE INDEX idx_api_keys_key_id ON api_keys (key_id);

-- Query: "list active keys for a tenant" (the only key-management list
-- view v0.19 needs). Partial (WHERE revoked_at IS NULL) keeps it small
-- forever, matching the outbox table's idx_outbox_pending precedent —
-- revoked keys accumulate but a tenant's active key list does not grow
-- with its full key history.
CREATE INDEX idx_api_keys_tenant_active ON api_keys (tenant_id) WHERE revoked_at IS NULL;
