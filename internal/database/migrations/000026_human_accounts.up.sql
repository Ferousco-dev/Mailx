-- v0.47 (Phase 1): human accounts, sessions, and org membership.
--
-- "Organization" is deliberately NOT a new concept: it is the existing
-- tenants table. A human "creates an organization" by creating a tenant
-- and becoming its owner via tenant_members — this reuses every
-- tenant-scoped table (messages, domains, api_keys, retention settings,
-- ...) with zero migration for them. See .ilana/decisions.md DEC-205.
--
-- humans are platform accounts, separate from api_keys (machine/service
-- credentials — see internal/auth/apikey.go's doc on why the two use
-- different hashing schemes: passwords are low-entropy and human-chosen,
-- so they need bcrypt, not the HMAC-pepper scheme used for random secrets).
CREATE TABLE humans (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    -- Case-insensitive uniqueness follows contacts.normalized_email's
    -- convention: a separate normalized column, unique-constrained,
    -- rather than a functional index or citext extension.
    normalized_email TEXT NOT NULL CHECK (length(normalized_email) BETWEEN 3 AND 254 AND normalized_email !~ '\s'),
    email          TEXT NOT NULL, -- as submitted, for display
    password_hash  TEXT NOT NULL, -- bcrypt
    role           TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('user','superadmin')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (normalized_email)
);

-- refresh_tokens: one row per issued refresh token. Mirrors api_keys'
-- rotate/revoke shape (see internal/auth/apikey.go RotateAPIKey/RevokeAPIKey):
-- only a hash is stored, rotation issues a new row and marks the old one
-- revoked, and reuse of an already-revoked token is the compromise signal
-- that revokes every other active token for that human (session-wide).
CREATE TABLE refresh_tokens (
    id          TEXT PRIMARY KEY,
    human_id    TEXT NOT NULL REFERENCES humans(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

-- Hot path: "verify by hash" on every refresh call.
CREATE UNIQUE INDEX idx_refresh_tokens_hash ON refresh_tokens (token_hash);
-- "revoke every active token for this human" (compromise response).
CREATE INDEX idx_refresh_tokens_human ON refresh_tokens (human_id) WHERE revoked_at IS NULL;

-- tenant_members: minimal org membership. Only enough to answer "who owns
-- this org" — not a full RBAC matrix (explicitly deferred, see DEC-205).
CREATE TABLE tenant_members (
    tenant_id   TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    human_id    TEXT NOT NULL REFERENCES humans(id) ON DELETE CASCADE,
    role        TEXT NOT NULL DEFAULT 'owner' CHECK (role IN ('owner','member')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, human_id)
);

-- "list orgs a human belongs to" (reverse direction of the PK).
CREATE INDEX idx_tenant_members_human ON tenant_members (human_id);
